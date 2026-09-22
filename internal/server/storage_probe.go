package server

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"time"

	"dingospeed/pkg/config"
	"dingospeed/pkg/dependency"
	"dingospeed/pkg/storageprobe"
	"github.com/prometheus/client_golang/prometheus"
)

type StorageProbeServer struct {
	config   *config.Config
	registry prometheus.Registerer
	mu       sync.Mutex
	probe    *storageprobe.Probe
	stopped  bool
	started  bool
}

func NewStorageProbeServer(c *config.Config) *StorageProbeServer {
	return &StorageProbeServer{config: c, registry: prometheus.DefaultRegisterer}
}

func (s *StorageProbeServer) Start(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped || s.started {
		return nil
	}
	if !s.config.StorageProbe.Enabled {
		if err := s.registry.Register(dependency.ImpactCollector(dependency.Default, nil)); err != nil {
			return err
		}
		s.started = true
		return nil
	}
	if s.config.Server.Repos == "" {
		return errors.New("storage probe requires server.repos")
	}
	// Only lexical resolution here: even startup must not await a broken mount.
	root, err := filepath.Abs(s.config.Server.Repos)
	if err != nil {
		return err
	}
	read, write := storageprobe.Filesystem(root)
	p, err := storageprobe.New(read, write, 10*time.Second, 3*time.Second)
	if err != nil {
		return err
	}
	probeCollector := storageprobe.Collector(p)
	if err = s.registry.Register(probeCollector); err != nil {
		return err
	}
	if err = s.registry.Register(dependency.ImpactCollector(dependency.Default, p.Evidence)); err != nil {
		s.registry.Unregister(probeCollector)
		return err
	}
	s.probe = p
	s.started = true
	p.Start(ctx)
	return nil
}

func (s *StorageProbeServer) Stop(context.Context) error {
	s.mu.Lock()
	s.stopped = true
	p := s.probe
	s.mu.Unlock()
	if p != nil {
		p.Stop()
	}
	return nil
}
