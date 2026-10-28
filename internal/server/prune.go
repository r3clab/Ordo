package server

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/r3clab/Ordo/internal/content"
	"github.com/r3clab/Ordo/internal/state"
)

const (
	pruneEvery = time.Hour
	// idle is how long an image or input nothing unfinished needs stays, on workers and the server, for
	// later jobs to reuse.
	idle = 24 * time.Hour
	// retention is how long the server keeps a finished job's record and logs; Out keeps its own copy.
	retention = 30 * 24 * time.Hour
)

// inUse names the images, image archives and input trees that unfinished jobs may still launch with.
func (s *Server) inUse() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var keep []string
	for _, j := range s.jobs {
		for _, stage := range j.Steps {
			keep = append(keep, stage.Spec.Image, stage.Archive)
			keep = slices.AppendSeq(keep, maps.Values(stage.Spec.Inputs))
		}
	}
	slices.Sort(keep)
	return slices.Compact(keep)
}

func (s *Server) tidy(ctx context.Context) {
	for {
		now := time.Now()
		if err := cmp.Or(s.pruneBlobs(now), s.expire(now)); err != nil {
			fmt.Fprintf(os.Stderr, "cleanup: %v\n", err)
		}
		if !sleep(ctx, pruneEvery) {
			return
		}
	}
}

// pruneBlobs skips a round that would wait for a submission still staging.
func (s *Server) pruneBlobs(now time.Time) error {
	if !s.staging.TryLock() {
		return nil
	}
	defer s.staging.Unlock()
	keep := map[string]bool{}
	for _, name := range s.inUse() {
		keep[name] = true
	}
	return content.Prune(s.images.Blobs, keep, now.Add(-idle))
}

// expire forgets finished jobs older than retention, except those an unfinished job takes inputs from,
// as it reads their state.
func (s *Server) expire(now time.Time) error {
	old, err := s.forgetOld(now)
	for _, id := range old {
		err = cmp.Or(err, os.RemoveAll(s.resultsDir(id)))
	}
	return err
}

func (s *Server) forgetOld(now time.Time) ([]string, error) {
	s.write.Lock()
	defer s.write.Unlock()
	s.mu.Lock()
	needed := map[string]bool{}
	for _, j := range s.jobs {
		for _, id := range j.After {
			needed[id] = true
		}
	}
	var old []string
	for id, r := range s.records {
		if r.State.Terminal() && !needed[id] && cmp.Or(r.FinishedAt, r.CreatedAt).Before(now.Add(-retention)) {
			old = append(old, id)
		}
	}
	s.mu.Unlock()
	if len(old) == 0 {
		return nil, nil
	}
	err := s.store.Update(func(tx *state.Tx) error {
		for _, id := range old {
			if err := tx.Delete("job", id); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	for _, id := range old {
		delete(s.records, id)
	}
	s.mu.Unlock()
	return old, nil
}

func (s *Server) resultsDir(id string) string { return filepath.Join(s.root, "results", id) }
