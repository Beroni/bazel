package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"time"
)

// RawStreamEnv names a folder that gets each step's stdout exactly as the agent
// wrote it, one file per step. It is for debugging what a CLI really says, which
// the log only shows translated. The files hold the PR's code: the folder is the
// user's and is created private.
const RawStreamEnv = "BAZEL_RAW_STREAM"

var rawUnsafeRe = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

type rawStream struct{ f *os.File }

// openRawStream opens the step's file when RawStreamEnv is set, or returns a
// stream that writes nothing.
func openRawStream(step string) *rawStream {
	dir := os.Getenv(RawStreamEnv)
	if dir == "" {
		return &rawStream{}
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		fmt.Fprintf(os.Stderr, "bazel: raw stream: %v\n", err)
		return &rawStream{}
	}
	name := fmt.Sprintf("%s-%s.jsonl", time.Now().Format("20060102-150405.000000"), rawUnsafeRe.ReplaceAllString(step, "-"))
	f, err := os.OpenFile(filepath.Join(dir, name), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		fmt.Fprintf(os.Stderr, "bazel: raw stream: %v\n", err)
		return &rawStream{}
	}
	return &rawStream{f: f}
}

func (r *rawStream) write(line string) {
	if r.f == nil {
		return
	}
	if _, err := fmt.Fprintln(r.f, line); err != nil {
		r.f.Close()
		r.f = nil
	}
}

func (r *rawStream) Close() {
	if r.f != nil {
		r.f.Close()
		r.f = nil
	}
}
