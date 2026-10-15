package replaytools

import (
	"context"
	"fmt"
	"maps"
	"os"
	"time"
)

// prepareRuntime lazily provisions a granted Profile and verifies source/readiness identity.
// A generic workspace starts uncompiled; registered scenarios may provide a baseline.
func (s *Server) prepareRuntime(ctx context.Context, profile string) error {
	setup := s.Config.Runtime
	if setup == nil {
		return fmt.Errorf("no registered runtime; request an environment change")
	}
	if err := os.MkdirAll(setup.Directory, 0700); err != nil {
		return err
	}
	call, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	path, cleanup, err := ManagedEnvironment(setup.Script, setup.Evidence, profile, setup.Workspace)(call, setup.Directory)
	if err != nil {
		return err
	}
	config, err := Load(path)
	if err != nil || config.Commit != s.Config.Commit || !maps.Equal(config.Files, s.Config.Files) {
		_ = cleanup()
		return fmt.Errorf("runtime readiness/source mismatch")
	}
	s.Config.Container, s.Config.Session, s.Config.DockerContext = config.Container, config.Session, config.DockerContext
	s.Config.Readiness = config.Readiness
	s.cleanup = cleanup
	return nil
}
