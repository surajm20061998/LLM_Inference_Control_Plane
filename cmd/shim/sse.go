/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"bytes"
	"encoding/json"
	"io"
	"time"
)

// sseDataPrefix is the field prefix of an SSE data line. The space after the
// colon is optional in the specification and every OpenAI-compatible server
// emits it, but a parser that requires it would silently see zero tokens
// against one that does not — and "zero tokens" is indistinguishable from
// "engine produced nothing", so the omission is tolerated explicitly.
var sseDataPrefix = []byte("data:")

// sseDone is the terminating payload of an OpenAI stream.
var sseDone = []byte("[DONE]")

// maxSSELine caps how much of a single unterminated line is buffered.
//
// Without a cap, an upstream that never emits a newline would grow this buffer
// until the shim is OOM-killed — and it would take the engine's pod with it,
// since they share a memory limit. Past the cap the line is dropped rather than
// buffered; losing a token count is a far smaller failure than losing the pod.
const maxSSELine = 1 << 20 // 1 MiB

// streamChunk is the subset of an OpenAI streaming frame the shim reads.
//
// Only the first choice is examined. n>1 sampling is not something this proxy
// tries to attribute per-choice: the client asked for one request and got one
// stream, and TTFT is a property of the stream.
type streamChunk struct {
	Usage   *completionUsage `json:"usage"`
	Choices []struct {
		Delta struct {
			Content string `json:"content"`
			// ReasoningContent is emitted by reasoning models — Qwen3 among
			// them — before any visible content. It is genuinely generated
			// output: the engine has produced a token and the user is waiting
			// no longer. Ignoring it would inflate TTFT by the entire length of
			// the reasoning block, which for this project's own default model
			// is most of the response.
			ReasoningContent string `json:"reasoning_content"`
			ToolCalls        []struct {
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`

		// Text is the legacy /v1/completions shape. Read so that the same
		// observer works on both endpoints.
		Text string `json:"text"`
	} `json:"choices"`
}

// hasToken reports whether this frame carries generated text.
func (c streamChunk) hasToken() bool {
	if len(c.Choices) == 0 {
		return false
	}
	ch := c.Choices[0]
	for _, call := range ch.Delta.ToolCalls {
		if call.Function.Name != "" || call.Function.Arguments != "" {
			return true
		}
	}
	return ch.Delta.Content != "" || ch.Delta.ReasoningContent != "" || ch.Text != ""
}

// A pointer distinguishes an explicit zero completion count from absent usage.
type completionUsage struct {
	CompletionTokens *int64 `json:"completion_tokens"`
}

func (s *requestState) observeUsage(u *completionUsage) {
	if u != nil && u.CompletionTokens != nil && *u.CompletionTokens >= 0 {
		s.reportedTokens, s.hasUsage = *u.CompletionTokens, true
	}
}

// streamObserver wraps an upstream SSE body and times the first generated
// token as the bytes pass through.
//
// # Why the measurement is taken here
//
// It is taken on READ from the upstream, before the bytes are written to the
// client. That is the earliest point at which the engine can be said to have
// produced the token, and it excludes the shim's own write and the client's
// consumption speed — a slow client must not be able to make the engine look
// slow, or a canary gate becomes a measurement of whoever happens to be calling
// it.
//
// # Why the first frame is not the first token
//
// Three things arrive before any generated text, and timing any of them
// measures how quickly the server said hello rather than how quickly it
// started thinking:
//
//   - The response HEADERS. llama.cpp sends them as soon as it accepts the
//     request, long before the model has processed the prompt.
//   - A role-only opening frame, `{"delta":{"role":"assistant"}}`, carrying no
//     text at all.
//   - Keep-alive comment lines (": ping"), which are not data frames.
//
// TTFT is therefore the first `data:` frame whose first choice carries
// non-empty content, reasoning content, or legacy text. Getting this wrong does
// not produce an obviously broken metric; it produces a plausible one that is
// consistently a few hundred milliseconds too low.
type streamObserver struct {
	upstream io.ReadCloser
	state    *requestState
	now      func() time.Time

	// buf holds bytes belonging to a line that has not terminated yet. A single
	// Read from the network is not a single SSE frame — a frame can span
	// several reads and several frames can arrive in one — so lines have to be
	// reassembled rather than assumed.
	buf bytes.Buffer

	// overlong marks that the current line exceeded maxSSELine and is being
	// discarded up to the next newline.
	overlong  bool
	data      bytes.Buffer
	dropEvent bool
	finished  bool
}

// newStreamObserver wraps body, recording observations into state.
func newStreamObserver(body io.ReadCloser, state *requestState, now func() time.Time) *streamObserver {
	return &streamObserver{upstream: body, state: state, now: now}
}

// Read copies from the upstream and scans what passed by.
//
// It never modifies, buffers or delays the bytes it returns. The caller — the
// reverse proxy's copy loop — gets exactly what the engine sent, at the moment
// it sent it, which is what keeps the shim transparent to streaming.
func (o *streamObserver) Read(p []byte) (int, error) {
	n, err := o.upstream.Read(p)
	if n > 0 {
		o.scan(p[:n])
	}
	if err == io.EOF && !o.finished {
		o.finished = true
		// SSE dispatches the final pending event when the connection closes even
		// when an upstream omitted the conventional trailing blank line. Process
		// it for telemetry without changing the bytes returned to the client.
		if o.overlong {
			o.dropEvent = true
		}
		if !o.overlong && o.buf.Len() > 0 {
			o.handleLine(o.buf.Bytes())
		}
		o.buf.Reset()
		if !o.dropEvent && o.data.Len() > 0 {
			o.handleEvent(o.data.Bytes())
		}
		o.data.Reset()
		o.dropEvent = false
	}
	if err != nil && err != io.EOF {
		o.state.readErr = err
	}
	return n, err
}

// Close releases the upstream body.
func (o *streamObserver) Close() error { return o.upstream.Close() }

// scan feeds newly read bytes through the line reassembler.
func (o *streamObserver) scan(chunk []byte) {
	for len(chunk) > 0 {
		idx := bytes.IndexByte(chunk, '\n')
		if idx < 0 {
			o.appendPartial(chunk)
			return
		}

		line := chunk[:idx]
		chunk = chunk[idx+1:]

		if o.overlong {
			// The tail of a line already abandoned for length. Resume at the
			// next one.
			o.overlong = false
			o.buf.Reset()
			o.dropEvent = true
			continue
		}
		if o.buf.Len()+len(line) > maxSSELine {
			o.buf.Reset()
			o.dropEvent = true
			continue
		}

		if o.buf.Len() > 0 {
			o.buf.Write(line)
			o.handleLine(o.buf.Bytes())
			o.buf.Reset()
			continue
		}
		o.handleLine(line)
	}
}

// appendPartial buffers an unterminated line fragment, enforcing maxSSELine.
func (o *streamObserver) appendPartial(chunk []byte) {
	if o.overlong {
		return
	}
	if o.buf.Len()+len(chunk) > maxSSELine {
		o.overlong = true
		o.buf.Reset()
		return
	}
	o.buf.Write(chunk)
}

// handleLine interprets one complete SSE line.
func (o *streamObserver) handleLine(line []byte) {
	// A trailing CR is legal in SSE and cpp-httplib emits one.
	line = bytes.TrimSuffix(line, []byte("\r"))
	if len(line) == 0 {
		if !o.dropEvent {
			o.handleEvent(o.data.Bytes())
		}
		o.data.Reset()
		o.dropEvent = false
		return
	}

	if !bytes.HasPrefix(line, sseDataPrefix) {
		// Comments (": ping"), other SSE fields (event:, id:, retry:) and the
		// blank line between frames all land here. None carry a token.
		return
	}

	payload := bytes.TrimPrefix(line[len(sseDataPrefix):], []byte(" "))
	if o.dropEvent {
		return
	}
	if o.data.Len()+len(payload)+1 > maxSSELine {
		o.dropEvent = true
		o.data.Reset()
		return
	}
	if o.data.Len() > 0 {
		o.data.WriteByte('\n')
	}
	o.data.Write(payload)
}

func (o *streamObserver) handleEvent(payload []byte) {
	payload = bytes.TrimSpace(payload)
	if len(payload) == 0 {
		return
	}
	if bytes.Equal(payload, sseDone) {
		o.state.sawDone = true
		return
	}

	var chunk streamChunk
	if err := json.Unmarshal(payload, &chunk); err != nil {
		// Upstreams add fields over time and some emit non-JSON keep-alives.
		// An unparseable frame is not an error worth failing a request over; it
		// simply carries no token we can attribute.
		return
	}
	o.state.observeUsage(chunk.Usage)
	if !chunk.hasToken() {
		return
	}

	now := o.now()
	if !o.state.sawFirstToken {
		o.state.sawFirstToken = true
		o.state.ttft = now.Sub(o.state.start)
	} else if o.state.interChunk != nil {
		o.state.interChunk(max(0, now.Sub(o.state.lastContent).Seconds()))
	}
	o.state.lastContent = now
	o.state.tokens++
}

// usageObserver copies at most one MiB for non-streaming usage inspection.
// Reads are returned immediately and unchanged; oversized bodies simply have
// no usage measurement. No prompt or completion text becomes a metric label.
type usageObserver struct {
	upstream io.ReadCloser
	state    *requestState
	buf      bytes.Buffer
	overlong bool
}

func (o *usageObserver) Read(p []byte) (int, error) {
	n, err := o.upstream.Read(p)
	if !o.overlong {
		if o.buf.Len()+n > maxSSELine {
			o.overlong = true
			o.buf.Reset()
		} else {
			o.buf.Write(p[:n])
		}
	}
	if err == io.EOF && !o.overlong {
		var response struct {
			Usage *completionUsage `json:"usage"`
		}
		if json.Unmarshal(o.buf.Bytes(), &response) == nil {
			o.state.observeUsage(response.Usage)
		}
	} else if err != nil && err != io.EOF {
		o.state.readErr = err
	}
	return n, err
}

func (o *usageObserver) Close() error { return o.upstream.Close() }
