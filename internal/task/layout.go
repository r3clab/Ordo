package task

import (
	"bytes"
	"encoding/json"
)

const (
	RecordFile     = "run.json"
	StdoutFile     = "stdout.log"
	StderrFile     = "stderr.log"
	OutputsDir     = "outputs"
	OutputsArchive = "outputs.tar"
)

var logFiles = []string{StdoutFile, StderrFile}

type State string

const (
	Preparing   State = "preparing"
	Running     State = "running"
	Interrupted State = "interrupted"
	Errored     State = "error"
	Finished    State = "finished"
)

// ContainerStatus is Podman's, or one of these when no container answers.
type ContainerStatus string

const (
	Unstarted ContainerStatus = "unstarted"
	Removed   ContainerStatus = "removed"
	Missing   ContainerStatus = "missing"
)

// Gone trusts a missing container only once its ID was saved: before that, an interrupted launch may
// still create it.
func (s ContainerStatus) Gone(r Record) bool {
	return s == Unstarted || s == Removed || (s == Missing && r.ContainerID != "")
}

func (r Record) Results() []string {
	files := []string{RecordFile, StdoutFile, StderrFile}
	if r.Request.ExportOutputs {
		files = append(files, OutputsArchive)
	}
	return files
}

// Same ignores the run and CDI directories, which are locations rather than identity, and compares
// secrets by name, the only part a record keeps.
func (r Request) Same(o Request) bool {
	r.OutputDir, o.OutputDir = "", ""
	r.CDISpecDir, o.CDISpecDir = "", ""
	a, errA := json.Marshal(r)
	b, errB := json.Marshal(o)
	return errA == nil && errB == nil && bytes.Equal(a, b)
}
