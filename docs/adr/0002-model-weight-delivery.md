# ADR-0002: Deliver model weights as an OCI image, with Hub download as the documented alternative

- **Status:** Accepted
- **Date:** 2026-09-02
- **Sprint:** 2
- **Supersedes:** the `ImageVolume` approach rejected during planning (see `research.md`)

## Context

A serving pod needs a GGUF file on disk before llama.cpp can load it. There are
four plausible ways to put it there, and the choice is not cosmetic: it decides
whether two replicas of the same revision are guaranteed to be running the same
bytes, which is the assumption every later sprint's canary comparison rests on.

1. **Bake the weights into the engine image.** Rejected outright. It welds the
   model's version to the server's, so "upgrade llama.cpp, keep the model" and
   "swap the model, keep llama.cpp" become the same operation — and Sprint 4
   needs to canary each of those independently. It also means a 400 MB rebuild
   to change a command-line flag.

2. **Kubernetes `ImageVolume` (`image:` volume source, GA in 1.33).** The
   obvious modern answer, and the one this project would use in a real cluster.
   It is unusable here: kind issue #4099 is open, and images side-loaded into a
   kind node are not visible at the image-volume mount path. Discovering that
   after building on it would have cost a sprint.

3. **An init container that copies from a model image.**

4. **Download at pod start from the Hugging Face Hub.**

## Decision

**Implement (3) as the recommended source and (4) as a documented alternative.
Do not implement either as the only one.**

The two are wired through a single seam, `buildModelDelivery` in
`internal/controller/children.go`, which returns *at most one* of `modelPath`
and `cacheDir`. Which one is set is what tells the engine profile whether the
model volume is mounted read-only or read-write. Encoding it as two mutually
exclusive fields rather than one path plus a mode flag means a profile cannot
ask for a writable mount it does not need.

**Image source (recommended).** The weights ship as their own OCI image built
on `busybox` — not `scratch`, because the init container's whole job is to run
`cp`, which `scratch` cannot provide. An init container copies the file onto a
shared `emptyDir`; the engine then mounts that **read-only**. The staging mount
path (`/mnt/model`) deliberately differs from the runtime mount path
(`/models`), because a volume mount shadows whatever the image already has at
that path — mounting the empty volume over the model image's own directory
would hide the very file being copied.

**Hugging Face source (alternative).** No init container at all: llama.cpp
already implements Hub downloads, including retry and resume, and
reimplementing that in an init container would mean owning code upstream has
already written. The engine is pointed at the Hub with `--hf-repo`/`--hf-file`
and `LLAMA_CACHE` is redirected onto the mounted volume — without that
redirection llama.cpp caches under `$HOME`, which is on the read-only root
filesystem, and the download fails with a permission error that reads like a
corrupt image.

`--hf-file` is **required**, not defaulted. A Hub repository carries every
quantisation side by side (Q4_K_M, Q8_0, BF16) and llama.cpp will otherwise
pick one by guessing. That guess is silent and it changes both the memory
footprint and the latency of every pod — which would mean a canary and its
primary could differ by a quantisation nobody declared, breaking the one thing
progressive delivery assumes is held constant.

A token for gated repositories is passed as `HF_TOKEN` sourced from a Secret,
never as `--hf-token`, so the credential never appears in the pod spec, in
`kubectl describe`, or in a process listing.

## Consequences

**Good.**
- Model and engine versions move independently, which is what makes Sprint 4's
  "canary the model" and "canary the engine" two separate demos.
- The recommended path is fully offline and deterministic: an immutable tag
  (`llmcp-model:qwen3-0.6b-q4km`) means two replicas created an hour apart load
  identical bytes, so `pullPolicy: IfNotPresent` is honest rather than risky.
- The engine container keeps a read-only root filesystem in both modes. Under
  the Hub source the *only* writable path in the pod is the model volume.
- Registry pushes are layer-incremental, so iterating on the operator does not
  re-transfer 400 MB the way `kind load docker-image` would.

**Bad, and accepted.**
- The Hub source re-downloads ~400 MB on every pod start, because the volume is
  an `emptyDir`. A restart, a rescheduling and a scale-up each pay it again.
  This is why it is documented as the alternative and not used for anything
  measured. A PVC-backed cache would fix it and is the natural next step, which
  is why `persistentVolumeClaim` already exists in the API as an unimplemented
  union member.
- The Hub source makes pod startup depend on upstream availability and rate
  limits, and on a tag that can move.
- The model image costs ~400 MB of local registry storage per model version.

**Neutral.**
- `persistentVolumeClaim` is modelled in the API but rejected by the engine
  profile with an explicit "not implemented yet" error. Adding a union member
  later is backward compatible; converting a scalar into a union is not, which
  is why all three were modelled on day one.

## Verification

- `TestLlamaCPPBuildHuggingFaceSource` — Hub flags present, `-m` absent, mount
  writable, `LLAMA_CACHE` on the volume, root filesystem still read-only.
- `TestLlamaCPPHuggingFaceTokenIsNotInTheArgs` — the credential is a Secret
  reference and never a command-line argument.
- `ModelDeployment controller when the model source is huggingFace` (envtest) —
  the rendered pod has no init container and a read-write model mount.
- Manually, on kind: `config/samples/qwen3_llamacpp.yaml` reaches
  `Ready` with three replicas and serves a streamed completion.
