package collect

import (
	"context"
	_ "embed"
	"time"

	"github.com/pgrundev/pgbot/internal/conn"
	"github.com/pgrundev/pgbot/internal/model"
	"github.com/pgrundev/pgbot/internal/rate"
)

//go:embed sql/io_bgwriter.sql
var sqlIOBgwriter string

//go:embed sql/io_checkpointer.sql
var sqlIOCheckpointer string

// io = checkpoint + buffers-written counters, double-sampled. PG17+ splits the
// counters into pg_stat_checkpointer + pg_stat_io; older versions keep them in
// pg_stat_bgwriter.
type ioCollector struct{}

type ioSample struct {
	CheckpointsTimed int64 `db:"checkpoints_timed"`
	CheckpointsReq   int64 `db:"checkpoints_req"`
	BuffersWritten   int64 `db:"buffers_written"`
	BackendFsyncs    int64 `db:"backend_fsyncs"`
}

func (ioCollector) Name() string                     { return "io" }
func (ioCollector) Kind() Kind                       { return KindCounter }
func (ioCollector) Available(conn.Capabilities) bool { return true }

func (ioCollector) Sample(ctx context.Context, t *conn.Target, caps conn.Capabilities) (any, error) {
	sql := sqlIOBgwriter
	if caps.HasStatCheckpointer() {
		sql = sqlIOCheckpointer
	}
	return queryOne[ioSample](ctx, t, sql)
}

func (ioCollector) Assemble(c *model.Context, _ conn.Capabilities, s sampled, dt time.Duration, _ Options) {
	dt = s.rateWindow(dt) // rates are divided by THIS collector's measured span, not the shared window (N4)
	a, aok := s.A.(ioSample)
	b, bok := s.B.(ioSample)
	if s.Err != nil || !aok || !bok {
		c.IO = &model.IO{Section: unavail(s.Err, "IO stats unavailable")}
		return
	}
	io := &model.IO{
		Section:          model.Section{Exactness: model.ExactnessSampled},
		CheckpointsTimed: b.CheckpointsTimed, CheckpointsReq: b.CheckpointsReq, BackendFsyncs: b.BackendFsyncs,
	}
	if bw, ok := rate.PerSecond(a.BuffersWritten, b.BuffersWritten, dt); ok {
		io.BuffersWrittenPerS = round2p(bw)
	}
	c.IO = io
}
