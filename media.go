package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	herald "github.com/jack-work/gluck-herald/client"
)

// Media is fetched here, on the laptop, rather than referenced.
//
// The split is forced by where things live. Herald holds the Telegram token
// and downloads on spain, because only it can; the aria runs here and reads
// files from this disk. A path on spain would be a path to nowhere. So the
// bridge pulls the bytes over the same authenticated connection it polls
// with, writes them locally, and hands the aria a path it can actually open.
//
// The fetch happens before the prompt is delivered and therefore before the
// message is acknowledged, which is what makes it safe: acknowledgement is
// what releases the bytes on spain, so a crash mid-fetch replays the message
// rather than losing the picture.

// mediaDir keeps attachments beside the bridge's other state.
func mediaDir() string {
	if d := strings.TrimSpace(os.Getenv("FIGARO_BRIDGE_MEDIA_DIR")); d != "" {
		return d
	}
	base := os.Getenv("XDG_STATE_HOME")
	if base == "" {
		base = filepath.Join(os.Getenv("HOME"), ".local", "state")
	}
	return filepath.Join(base, "figaro-bridge", "media")
}

// maxMediaAge bounds what accumulates here. The aria reads an image during
// the turn it arrives in; a week later it is litter on a laptop.
const maxMediaAge = 7 * 24 * time.Hour

// fetchMedia downloads a message's attachments and returns lines describing
// them for the aria, or "" when the message carries none.
//
// A failure is described rather than hidden. An aria told the image did not
// arrive can ask for it again; an aria told nothing believes nothing was
// sent, which is precisely the bug this feature exists to fix.
func (b *bridge) fetchMedia(ctx context.Context, m herald.Message) string {
	if len(m.Media) == 0 {
		return ""
	}
	dir := mediaDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		log.Printf("media dir: %v", err)
		return "[an attachment arrived but could not be saved: " + err.Error() + "]"
	}
	sweepMedia(dir)

	var notes []string
	for _, md := range m.Media {
		if md.Error != "" {
			log.Printf("media %s: herald reported %q", md.Kind, md.Error)
			notes = append(notes, fmt.Sprintf("[the user sent a %s, but it could not be downloaded: %s]", md.Kind, md.Error))
			continue
		}
		if md.ID == "" {
			continue
		}
		path := filepath.Join(dir, md.ID)
		f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
		if err != nil {
			log.Printf("media %s: %v", md.ID, err)
			notes = append(notes, "[an attachment arrived but could not be saved: "+err.Error()+"]")
			continue
		}
		fetchCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		n, err := b.herald.Media(fetchCtx, md.ID, f)
		cancel()
		cerr := f.Close()
		if err == nil {
			err = cerr
		}
		if err != nil {
			os.Remove(path)
			log.Printf("media %s: fetch failed: %v", md.ID, err)
			notes = append(notes, fmt.Sprintf("[the user sent a %s, but fetching it from herald failed: %s]", md.Kind, err))
			continue
		}
		log.Printf("media %s saved %s (%d bytes)", md.Kind, path, n)
		notes = append(notes, describe(md, path))
	}
	return strings.Join(notes, "\n")
}

// describe writes the line the aria actually reads. It names the read tool
// explicitly: a path alone is an invitation to guess, and the whole point is
// that the model should look at the picture.
func describe(md herald.Media, path string) string {
	switch md.Kind {
	case "photo":
		size := ""
		if md.Width > 0 && md.Height > 0 {
			size = fmt.Sprintf(" (%dx%d)", md.Width, md.Height)
		}
		return fmt.Sprintf("[the user sent an image%s, saved to %s — use the read tool to view it]", size, path)
	default:
		name := md.Name
		if name == "" {
			name = "a file"
		}
		return fmt.Sprintf("[the user sent %q, saved to %s — use the read tool to view it]", name, path)
	}
}

// sweepMedia deletes what nobody came back for. Best effort by design: a
// failure here must never cost the message that triggered it.
func sweepMedia(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-maxMediaAge)
	for _, e := range entries {
		fi, err := e.Info()
		if err != nil || e.IsDir() {
			continue
		}
		if fi.ModTime().Before(cutoff) {
			os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}
