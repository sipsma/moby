package exporter

import (
	"context"

	"github.com/moby/buildkit/cache"
	"github.com/moby/buildkit/solver"
)

type Exporter interface {
	Resolve(context.Context, map[string]string) (ExporterInstance, error)
}

type ExporterInstance interface {
	Name() string
	Config() Config
	Export(ctx context.Context, src Source, sessionID string) (map[string]string, error)
}

type Source struct {
	Ref      cache.ImmutableRef
	Refs     map[string]cache.ImmutableRef
	Metadata map[string][]byte
}

type Config struct {
	Compression solver.CompressionOpt
}
