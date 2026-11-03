package cli

import (
	"bufio"
	"bytes"
	"encoding/json"
	"sync"

	"github.com/stretchr/testify/require"

	"github.com/mrz1836/mixtape/internal/library"
)

// playlistSpec is the library collection of the test playlist (plID).
func playlistSpec(name string) library.Spec {
	return library.Spec{Kind: library.KindPlaylist, SpotifyID: plID, Name: name}
}

// warnings decodes the JSON warnings on stderr, in order.
func (h *harness) warnings() []string {
	h.t.Helper()
	var out []string
	sc := bufio.NewScanner(bytes.NewReader(h.stderr.Bytes()))
	for sc.Scan() {
		var doc struct {
			Warning *struct {
				Message string `json:"message"`
			} `json:"warning"`
		}
		require.NoError(h.t, json.Unmarshal(sc.Bytes(), &doc), "stderr line: %s", sc.Text())
		if doc.Warning != nil {
			out = append(out, doc.Warning.Message)
		}
	}
	return out
}

// editLibrary opens the harness library under its writer lock and hands
// the collection for spec to edit, then saves it, as another mixtape run
// would have left it.
func (h *harness) editLibrary(spec library.Spec, edit func(c *library.Collection)) {
	h.t.Helper()
	t := h.t
	lib, err := library.Open(h.env["MIXTAPE_LIBRARY"], library.Options{Encoder: seedEncoder(), WorkDir: t.TempDir()})
	require.NoError(t, err)
	unlock, err := lib.Lock()
	require.NoError(t, err)
	defer unlock()
	c, err := lib.Collection(spec)
	require.NoError(t, err)
	edit(c)
	require.NoError(t, lib.Save(c))
}

// syncBuffer is a bytes.Buffer safe for a writer goroutine and a reader.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
