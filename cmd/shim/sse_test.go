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
	"io"
	"strings"
	"testing"
	"time"
)

// fakeClock returns a now() that advances by step on every call, starting at
// base. It makes TTFT a countable quantity instead of a wall-clock measurement,
// which is what lets these tests assert exact durations.
func fakeClock(base time.Time, step time.Duration) func() time.Time {
	n := 0
	return func() time.Time {
		t := base.Add(time.Duration(n) * step)
		n++
		return t
	}
}

// drain reads a stream observer to completion in chunks, so that the line
// reassembler is exercised across read boundaries rather than handed one
// perfectly-formed buffer.
func drain(t *testing.T, o *streamObserver, chunkSize int) {
	t.Helper()
	buf := make([]byte, chunkSize)
	for {
		_, err := o.Read(buf)
		if err == io.EOF {
			return
		}
		if err != nil {
			t.Fatalf("read: %v", err)
		}
	}
}

// llamaCPPStream is a recorded-shape llama.cpp chat stream: a role-only opening
// frame, then content frames, then the terminator.
const llamaCPPStream = `data: {"choices":[{"delta":{"role":"assistant"},"index":0,"finish_reason":null}]}

data: {"choices":[{"delta":{"content":"Hello"},"index":0,"finish_reason":null}]}

data: {"choices":[{"delta":{"content":" world"},"index":0,"finish_reason":null}]}

data: {"choices":[{"delta":{},"index":0,"finish_reason":"stop"}]}

data: [DONE]

`

func TestStreamObserverTTFTSkipsRoleFrame(t *testing.T) {
	// One tick per now() call: start, then the TTFT sample.
	state := &requestState{start: time.Unix(0, 0)}
	clock := fakeClock(time.Unix(0, 0), 100*time.Millisecond)
	// Consume the first tick as "start" so the next one is the observation.
	_ = clock()

	o := newStreamObserver(io.NopCloser(strings.NewReader(llamaCPPStream)), state, clock)
	drain(t, o, 7)

	if !state.sawFirstToken {
		t.Fatal("expected a first token to be observed")
	}
	if state.tokens != 2 {
		t.Fatalf("tokens = %d, want 2 (the role frame and the finish frame carry no text)", state.tokens)
	}
	if !state.sawDone {
		t.Fatal("expected the [DONE] terminator to be seen")
	}
	// The role frame must NOT have set TTFT. With a 100ms-per-call clock the
	// observation lands on the second call, i.e. 100ms after start.
	if state.ttft != 100*time.Millisecond {
		t.Fatalf("ttft = %v, want 100ms; a nonzero-but-different value means the role frame was timed", state.ttft)
	}
}

func TestStreamObserverSplitsAcrossReads(t *testing.T) {
	// Read one byte at a time: every frame is guaranteed to span several reads.
	for _, chunk := range []int{1, 2, 3, 13, 64, 4096} {
		state := &requestState{start: time.Unix(0, 0)}
		clock := fakeClock(time.Unix(0, 0), time.Millisecond)
		_ = clock()

		o := newStreamObserver(io.NopCloser(strings.NewReader(llamaCPPStream)), state, clock)
		drain(t, o, chunk)

		if state.tokens != 2 {
			t.Fatalf("chunk=%d: tokens = %d, want 2", chunk, state.tokens)
		}
		if !state.sawDone {
			t.Fatalf("chunk=%d: [DONE] not seen", chunk)
		}
	}
}

func TestStreamObserverPassesBytesThroughUnchanged(t *testing.T) {
	state := &requestState{start: time.Unix(0, 0)}
	o := newStreamObserver(io.NopCloser(strings.NewReader(llamaCPPStream)), state, time.Now)

	got, err := io.ReadAll(o)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if string(got) != llamaCPPStream {
		t.Fatal("observer altered the stream; it must be byte-transparent")
	}
}

func TestStreamObserverCountsReasoningContent(t *testing.T) {
	// Qwen3 and other reasoning models emit reasoning_content before any
	// visible content. Those are generated tokens: the user has stopped
	// waiting. Ignoring them would inflate TTFT by the whole reasoning block.
	const stream = `data: {"choices":[{"delta":{"role":"assistant"}}]}

data: {"choices":[{"delta":{"reasoning_content":"thinking"}}]}

data: {"choices":[{"delta":{"content":"answer"}}]}

data: [DONE]

`
	state := &requestState{start: time.Unix(0, 0)}
	clock := fakeClock(time.Unix(0, 0), 50*time.Millisecond)
	_ = clock()

	o := newStreamObserver(io.NopCloser(strings.NewReader(stream)), state, clock)
	drain(t, o, 8)

	if state.tokens != 2 {
		t.Fatalf("tokens = %d, want 2", state.tokens)
	}
	if state.ttft != 50*time.Millisecond {
		t.Fatalf("ttft = %v, want 50ms (the reasoning frame is the first token)", state.ttft)
	}
}

func TestStreamObserverLegacyCompletionShape(t *testing.T) {
	const stream = `data: {"choices":[{"text":"tok ","index":0}]}

data: {"choices":[{"text":"","index":0,"finish_reason":"stop"}]}

data: [DONE]

`
	state := &requestState{start: time.Unix(0, 0)}
	o := newStreamObserver(io.NopCloser(strings.NewReader(stream)), state, time.Now)
	drain(t, o, 16)

	if state.tokens != 1 {
		t.Fatalf("tokens = %d, want 1", state.tokens)
	}
}

func TestStreamObserverIgnoresNoise(t *testing.T) {
	// Comments, unknown SSE fields, CRLF terminators, unparseable payloads and
	// a data line with no space after the colon. None of these carry a token
	// and none may abort the scan.
	const stream = ": keep-alive\r\n" +
		"event: message\r\n" +
		"id: 42\r\n" +
		"data: not json at all\r\n" +
		"\r\n" +
		"data:{\"choices\":[{\"delta\":{\"content\":\"x\"}}]}\r\n" +
		"\r\n" +
		"data: [DONE]\r\n\r\n"

	state := &requestState{start: time.Unix(0, 0)}
	o := newStreamObserver(io.NopCloser(strings.NewReader(stream)), state, time.Now)
	drain(t, o, 5)

	if state.tokens != 1 {
		t.Fatalf("tokens = %d, want 1", state.tokens)
	}
	if !state.sawDone {
		t.Fatal("[DONE] not seen")
	}
}

func TestStreamObserverCapsOverlongLine(t *testing.T) {
	// An upstream that never terminates a line must not be able to grow the
	// buffer without bound. The overlong line is dropped; the frame after it is
	// still counted, which proves the reassembler resynchronises.
	var b strings.Builder
	b.WriteString("data: ")
	b.WriteString(strings.Repeat("A", maxSSELine+1024))
	b.WriteString("\n\n")
	b.WriteString(`data: {"choices":[{"delta":{"content":"x"}}]}` + "\n\n")
	b.WriteString("data: [DONE]\n\n")

	state := &requestState{start: time.Unix(0, 0)}
	o := newStreamObserver(io.NopCloser(strings.NewReader(b.String())), state, time.Now)
	drain(t, o, 4096)

	if state.tokens != 1 {
		t.Fatalf("tokens = %d, want 1 after resynchronising past the overlong line", state.tokens)
	}
	if o.buf.Len() > maxSSELine {
		t.Fatalf("buffer grew to %d bytes, past the %d cap", o.buf.Len(), maxSSELine)
	}
}

func TestStreamObserverMultipleFramesInOneRead(t *testing.T) {
	state := &requestState{start: time.Unix(0, 0)}
	o := newStreamObserver(io.NopCloser(strings.NewReader(llamaCPPStream)), state, time.Now)
	// A buffer larger than the whole stream: every frame arrives in one Read.
	drain(t, o, len(llamaCPPStream)*2)

	if state.tokens != 2 {
		t.Fatalf("tokens = %d, want 2", state.tokens)
	}
}

func TestIsEventStream(t *testing.T) {
	cases := map[string]bool{
		"text/event-stream":                true,
		"text/event-stream; charset=utf-8": true,
		"TEXT/EVENT-STREAM":                true,
		"application/json":                 false,
		"":                                 false,
		// Malformed parameters still fall back to a prefix match rather than
		// classifying an obvious SSE body as non-streaming.
		"text/event-stream;;;": true,
	}
	for header, want := range cases {
		if got := isEventStream(header); got != want {
			t.Errorf("isEventStream(%q) = %v, want %v", header, got, want)
		}
	}
}
