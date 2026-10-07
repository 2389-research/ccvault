// ABOUTME: Syncing view for ccvault TUI
// ABOUTME: Shows sync progress with a progress bar

package tui

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"github.com/2389-research/ccvault/internal/config"
	"github.com/2389-research/ccvault/internal/db"
	"github.com/2389-research/ccvault/internal/sync"
	"github.com/charmbracelet/bubbles/progress"
	tea "github.com/charmbracelet/bubbletea"
)

// syncCountProgress carries numeric progress from the sync goroutine
type syncCountProgress struct {
	current int
	total   int
}

// SyncingModel holds syncing view state
type SyncingModel struct {
	db        *db.DB
	sources   []config.SourceConfig
	progress  []string
	done      bool
	err       error
	stats     *sync.Stats
	startTime time.Time
	bar       progress.Model
	current   int
	total     int

	// Channels for communicating with the sync goroutine
	progressCh chan string
	countCh    chan syncCountProgress
	doneCh     chan syncCompleteMsg

	// Lifecycle of the sync goroutine. ctx is cancelled when the user leaves
	// the syncing view or force-quits; exited closes once the goroutine has
	// returned, which is the signal the caller needs before closing the
	// database underneath it.
	// started is atomic because the shutdown path reads it from the goroutine
	// that ran the Bubble Tea program, not the one that drove Update.
	ctx     context.Context
	cancel  context.CancelFunc
	started atomic.Bool
	exited  chan struct{}
}

// NewSyncingModel creates a new syncing model
func NewSyncingModel(database *db.DB, sources []config.SourceConfig) *SyncingModel {
	bar := progress.New(
		progress.WithDefaultGradient(),
		progress.WithWidth(40),
		progress.WithoutPercentage(),
	)
	ctx, cancel := context.WithCancel(context.Background())
	return &SyncingModel{
		db:         database,
		sources:    sources,
		progress:   []string{},
		startTime:  time.Now(),
		bar:        bar,
		progressCh: make(chan string, 100),
		countCh:    make(chan syncCountProgress, 100),
		doneCh:     make(chan syncCompleteMsg, 1),
		ctx:        ctx,
		cancel:     cancel,
		exited:     make(chan struct{}),
	}
}

// syncCompleteMsg is sent when sync completes
type syncCompleteMsg struct {
	stats *sync.Stats
	err   error
}

// syncTickMsg triggers a UI refresh to show elapsed time and new progress
type syncTickMsg struct{}

// Init starts the sync operation
func (m *SyncingModel) Init() tea.Cmd {
	if m.started.Swap(true) {
		// A second goroutine over the same channels would double-close exited
		// and crash the program. Callers get a fresh model per sync instead.
		return m.tick()
	}

	m.startTime = time.Now()
	m.progress = []string{"Starting sync..."}

	// Launch sync in a goroutine so the TUI stays responsive
	go func() {
		defer close(m.exited)

		syncer := sync.New(m.db, m.sources,
			// Both callbacks block on a full channel, and the channels are only
			// drained while this view is on screen. Selecting on ctx.Done() is
			// what lets the goroutine walk away once nobody is listening —
			// cancelling the context alone cannot unblock a channel send.
			sync.WithProgressCallback(func(msg string) {
				select {
				case m.progressCh <- msg:
				case <-m.ctx.Done():
				}
			}),
			sync.WithCountProgressCallback(func(current, total int) {
				select {
				case m.countCh <- syncCountProgress{current: current, total: total}:
				case <-m.ctx.Done():
				}
			}),
		)

		stats, err := syncer.Run(m.ctx)
		// doneCh is buffered, so this never blocks even with no reader left.
		m.doneCh <- syncCompleteMsg{stats: stats, err: err}
	}()

	return m.tick()
}

// Cancel asks the sync goroutine to stop. Safe to call more than once, and
// safe on a model whose sync never started.
func (m *SyncingModel) Cancel() {
	m.cancel()
}

// WaitForExit blocks until the sync goroutine has returned, or until timeout.
// Reports whether the goroutine is gone. Callers use this before closing the
// database so a cancelled sync cannot be mid-transaction when the connection
// disappears underneath it.
func (m *SyncingModel) WaitForExit(timeout time.Duration) bool {
	if !m.started.Load() {
		return true
	}
	select {
	case <-m.exited:
		return true
	case <-time.After(timeout):
		return false
	}
}

// tick returns a command that fires a syncTickMsg after a short delay
func (m *SyncingModel) tick() tea.Cmd {
	return tea.Tick(200*time.Millisecond, func(t time.Time) tea.Msg {
		return syncTickMsg{}
	})
}

// Update handles syncing view events
func (m *SyncingModel) Update(msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case syncTickMsg:
		// Drain any pending progress messages
		for {
			select {
			case p := <-m.progressCh:
				m.progress = append(m.progress, p)
			default:
				goto drainedText
			}
		}
	drainedText:

		// Drain count progress
		for {
			select {
			case c := <-m.countCh:
				m.current = c.current
				m.total = c.total
			default:
				goto drainedCount
			}
		}
	drainedCount:

		// Check if sync completed
		select {
		case result := <-m.doneCh:
			m.done = true
			m.stats = result.stats
			m.err = result.err
			// Return the completion message so app.go can handle the transition
			return func() tea.Msg {
				return syncCompleteMsg{stats: result.stats, err: result.err}
			}
		default:
		}

		// Keep ticking while the sync is running. The chain no longer dies when
		// the user leaves the view (#99), so cancellation is what has to end it:
		// a cancelled sync has nothing left to drain, and a tick every 200ms for
		// the rest of the session would be the only thing still asking.
		if m.ctx.Err() != nil {
			return nil
		}
		return m.tick()

	case syncCompleteMsg:
		m.done = true
		m.stats = msg.stats
		m.err = msg.err
		return nil

	// Handle progress bar animation frames
	case progress.FrameMsg:
		model, cmd := m.bar.Update(msg)
		m.bar = model.(progress.Model)
		return cmd
	}
	return nil
}

// View renders the syncing view
func (m *SyncingModel) View() string {
	var b strings.Builder

	b.WriteString(titleStyle.Render("Syncing Conversations"))
	b.WriteString("\n\n")

	if m.err != nil {
		b.WriteString(errorStyle.Render("Error: " + m.err.Error()))
		b.WriteString("\n\n")
		b.WriteString(helpStyle.Render("Press any key to continue..."))
		return b.String()
	}

	if m.done && m.stats != nil {
		b.WriteString(statValueStyle.Render("Sync Complete!"))
		b.WriteString("\n\n")

		// Show full progress bar
		b.WriteString("  ")
		b.WriteString(m.bar.ViewAs(1.0))
		b.WriteString("\n\n")

		b.WriteString(statLabelStyle.Render("Projects:  "))
		b.WriteString(statValueStyle.Render(formatNumber(m.stats.ProjectsFound)))
		b.WriteString("\n")
		b.WriteString(statLabelStyle.Render("Sessions:  "))
		b.WriteString(statValueStyle.Render(formatNumber(m.stats.SessionsIndexed) + " indexed"))
		if m.stats.SessionsSkipped > 0 {
			b.WriteString(dimStyle.Render(", " + formatNumber(m.stats.SessionsSkipped) + " unchanged"))
		}
		b.WriteString("\n")
		b.WriteString(statLabelStyle.Render("Turns:     "))
		b.WriteString(statValueStyle.Render(formatNumber(m.stats.TurnsIndexed)))
		b.WriteString("\n")
		b.WriteString(statLabelStyle.Render("Duration:  "))
		b.WriteString(statValueStyle.Render(m.stats.Duration.Round(time.Millisecond).String()))
		b.WriteString("\n\n")
		b.WriteString(helpStyle.Render("Press any key to continue..."))
		return b.String()
	}

	// Show progress bar and elapsed time
	elapsed := time.Since(m.startTime).Round(time.Second)

	if m.total > 0 {
		pct := float64(m.current) / float64(m.total)
		b.WriteString("  ")
		b.WriteString(m.bar.ViewAs(pct))
		fmt.Fprintf(&b, "  %d/%d", m.current, m.total)
		b.WriteString("\n\n")
	}

	b.WriteString(dimStyle.Render("Scanning conversations... " + elapsed.String()))
	b.WriteString("\n\n")

	// Show last few progress messages
	start := 0
	if len(m.progress) > 5 {
		start = len(m.progress) - 5
	}
	for _, msg := range m.progress[start:] {
		b.WriteString(dimStyle.Render("  " + msg))
		b.WriteString("\n")
	}

	// A running sync is interruptible, which was not discoverable anywhere:
	// esc/q leaves the view and cancels the sync, ctrl+c quits outright.
	b.WriteString("\n")
	b.WriteString(helpStyle.Render("esc/q: back • ctrl+c: quit"))

	return b.String()
}

// IsDone returns whether sync is complete
func (m *SyncingModel) IsDone() bool {
	return m.done
}

// HasError returns whether sync encountered an error
func (m *SyncingModel) HasError() bool {
	return m.err != nil
}

// formatNumber formats an integer for display
func formatNumber(n int) string {
	return formatCompact(int64(n))
}
