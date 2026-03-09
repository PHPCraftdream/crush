package format

import (
	"fmt"
	"os"
	"time"

	"github.com/charmbracelet/x/ansi"
)

var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// Spinner displays an animated spinner on stderr while work is in progress.
type Spinner struct {
	stop chan struct{}
	done chan struct{}
}

// NewSpinner creates a new Spinner.
func NewSpinner() *Spinner {
	return &Spinner{
		stop: make(chan struct{}),
		done: make(chan struct{}),
	}
}

// Start begins the spinner animation in the background.
func (s *Spinner) Start() {
	go func() {
		defer close(s.done)
		ticker := time.NewTicker(80 * time.Millisecond)
		defer ticker.Stop()
		i := 0
		for {
			select {
			case <-s.stop:
				_, _ = fmt.Fprint(os.Stderr, "\r"+ansi.EraseEntireLine)
				return
			case <-ticker.C:
				_, _ = fmt.Fprintf(os.Stderr, "\r%s Generating...", spinnerFrames[i%len(spinnerFrames)])
				i++
			}
		}
	}()
}

// Stop ends the spinner animation and clears the line.
func (s *Spinner) Stop() {
	select {
	case <-s.stop:
	default:
		close(s.stop)
	}
	<-s.done
}
