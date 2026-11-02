package spotify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Catalog endpoints and limits.
const (
	defaultEmbedBase  = "https://open.spotify.com/embed"
	defaultOEmbedBase = "https://open.spotify.com/oembed"
	defaultUserAgent  = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 " +
		"(KHTML, like Gecko) Chrome/130.0 Safari/537.36"
	catalogTimeout = 15 * time.Second
	maxEmbedBody   = 5 << 20
	maxOEmbedBody  = 1 << 20
	// embedTrackCap is the most tracks the embed page lists.
	embedTrackCap = 100
)

// Source values for Collection.Source.
const (
	SourceEmbed  = "embed"
	SourceOEmbed = "oembed"
)

// nextDataStart and nextDataEnd bracket the page's JSON state.
const (
	nextDataStart = `<script id="__NEXT_DATA__" type="application/json">`
	nextDataEnd   = `</script>`
)

// Internal failure details; Resolve wraps them in ErrCatalog.
var (
	// errEmbed reports an embed page Mixtape could not use; Resolve then
	// falls back to oEmbed.
	errEmbed         = errors.New("embed page unusable")
	errOEmbedStatus  = errors.New("oembed: unexpected status")
	errOEmbedNoTitle = errors.New("oembed: no title in response")
)

// Catalog resolves Spotify links to their name and track list from
// Spotify's public pages: no credentials, no API keys.
//
// The track list comes from the public, undocumented embed page
// (open.spotify.com/embed/…), which lists at most 100 tracks. If that page
// changes shape, Resolve falls back to the documented oEmbed endpoint,
// which yields the name alone; the recorder then follows the playlist in
// the app instead of working from a list.
type Catalog struct {
	Client     *http.Client
	EmbedBase  string
	OEmbedBase string
	UserAgent  string
}

// NewCatalog returns a Catalog for the live Spotify endpoints.
func NewCatalog() *Catalog {
	return &Catalog{
		Client:     &http.Client{Timeout: catalogTimeout},
		EmbedBase:  defaultEmbedBase,
		OEmbedBase: defaultOEmbedBase,
		UserAgent:  defaultUserAgent,
	}
}

// Resolve returns the collection ref points at. A link Spotify does not
// know is ErrNotFound; when both the embed page and oEmbed fail the error
// wraps ErrCatalog.
func (c *Catalog) Resolve(ctx context.Context, ref Ref) (Collection, error) {
	col, err := c.embed(ctx, ref)
	if err == nil {
		return col, nil
	}
	if errors.Is(err, ErrNotFound) {
		return Collection{}, err
	}
	name, oerr := c.oembedTitle(ctx, ref)
	if oerr != nil {
		if errors.Is(oerr, ErrNotFound) {
			return Collection{}, oerr
		}
		return Collection{}, fmt.Errorf("%w: %s: embed: %w; oembed: %w", ErrCatalog, ref.URI(), err, oerr)
	}
	col = Collection{Ref: ref, Name: name, Source: SourceOEmbed}
	if ref.Kind == KindTrack {
		col.Tracks = []CatalogTrack{{ID: ref.ID, Title: name, Playable: true, Position: 1}}
	}
	return col, nil
}

// embedEntity is the part of the embed page's JSON Mixtape reads.
type embedEntity struct {
	Type       string `json:"type"`
	Name       string `json:"name"`
	Title      string `json:"title"`
	Subtitle   string `json:"subtitle"`
	Duration   int64  `json:"duration"`
	IsPlayable *bool  `json:"isPlayable"`
	Artists    []struct {
		Name string `json:"name"`
	} `json:"artists"`
	CoverArt struct {
		Sources []embedImage `json:"sources"`
	} `json:"coverArt"`
	VisualIdentity struct {
		Image []embedImage `json:"image"`
	} `json:"visualIdentity"`
	TrackList []struct {
		URI        string `json:"uri"`
		Title      string `json:"title"`
		Subtitle   string `json:"subtitle"`
		Duration   int64  `json:"duration"`
		IsPlayable *bool  `json:"isPlayable"`
	} `json:"trackList"`
}

type embedImage struct {
	URL       string `json:"url"`
	Width     *int   `json:"width"`
	MaxWidth  *int   `json:"maxWidth"`
	Height    *int   `json:"height"`
	MaxHeight *int   `json:"maxHeight"`
}

type embedPage struct {
	Props struct {
		PageProps struct {
			Status int `json:"status"`
			State  *struct {
				Data struct {
					Entity *embedEntity `json:"entity"`
				} `json:"data"`
			} `json:"state"`
		} `json:"pageProps"`
	} `json:"props"`
}

// embed fetches and parses the embed page for ref.
func (c *Catalog) embed(ctx context.Context, ref Ref) (Collection, error) {
	endpoint := strings.TrimRight(c.embedBase(), "/") + "/" + string(ref.Kind) + "/" + ref.ID
	body, status, err := c.get(ctx, endpoint, maxEmbedBody)
	if err != nil {
		return Collection{}, err
	}
	if status == http.StatusNotFound {
		return Collection{}, fmt.Errorf("%w: %s", ErrNotFound, ref.URI())
	}
	if status != http.StatusOK {
		return Collection{}, fmt.Errorf("%w: status %d", errEmbed, status)
	}
	page, err := parseEmbedPage(body)
	if err != nil {
		return Collection{}, err
	}
	pp := page.Props.PageProps
	if pp.Status == http.StatusNotFound {
		return Collection{}, fmt.Errorf("%w: %s", ErrNotFound, ref.URI())
	}
	if pp.State == nil || pp.State.Data.Entity == nil {
		return Collection{}, fmt.Errorf("%w: no entity in page state", errEmbed)
	}
	return collectionFromEntity(ref, pp.State.Data.Entity)
}

// parseEmbedPage extracts and decodes the __NEXT_DATA__ JSON.
func parseEmbedPage(body []byte) (embedPage, error) {
	_, after, ok := bytes.Cut(body, []byte(nextDataStart))
	if !ok {
		return embedPage{}, fmt.Errorf("%w: no __NEXT_DATA__ script", errEmbed)
	}
	data, _, ok := bytes.Cut(after, []byte(nextDataEnd))
	if !ok {
		return embedPage{}, fmt.Errorf("%w: unterminated __NEXT_DATA__ script", errEmbed)
	}
	var page embedPage
	if err := json.Unmarshal(data, &page); err != nil {
		return embedPage{}, fmt.Errorf("%w: %w", errEmbed, err)
	}
	return page, nil
}

// collectionFromEntity converts the page entity into a Collection.
func collectionFromEntity(ref Ref, e *embedEntity) (Collection, error) {
	if e.Type != "" && e.Type != string(ref.Kind) {
		return Collection{}, fmt.Errorf("%w: entity type %q for a %s link", errEmbed, e.Type, ref.Kind)
	}
	col := Collection{
		Ref:        ref,
		Name:       firstNonEmpty(e.Name, e.Title),
		ArtworkURL: largestImage(append(e.CoverArt.Sources, e.VisualIdentity.Image...)),
		Source:     SourceEmbed,
	}
	if col.Name == "" {
		return Collection{}, fmt.Errorf("%w: entity has no name", errEmbed)
	}

	if ref.Kind == KindTrack {
		artists := make([]string, 0, len(e.Artists))
		for _, a := range e.Artists {
			if a.Name != "" {
				artists = append(artists, a.Name)
			}
		}
		col.Tracks = []CatalogTrack{{
			ID:       ref.ID,
			Title:    col.Name,
			Artist:   strings.Join(artists, ", "),
			Duration: time.Duration(e.Duration) * time.Millisecond,
			Playable: boolOr(e.IsPlayable, true),
			Position: 1,
		}}
		col.Complete = true
		return col, nil
	}

	if len(e.TrackList) == 0 {
		// An album always has songs, and a playlist page without any is far
		// more likely a changed page than an empty playlist: never take it
		// for a complete, empty list (that would mark every song removed).
		return Collection{}, fmt.Errorf("%w: no track list in the page", errEmbed)
	}
	col.Owner = e.Subtitle
	for _, t := range e.TrackList {
		id := parseTrackURI(t.URI)
		if id == "" {
			continue // local files, episodes, anything that is not a track
		}
		col.Tracks = append(col.Tracks, CatalogTrack{
			ID:       id,
			Title:    t.Title,
			Artist:   t.Subtitle,
			Duration: time.Duration(t.Duration) * time.Millisecond,
			Playable: boolOr(t.IsPlayable, true),
			Position: len(col.Tracks) + 1,
		})
	}
	col.Truncated = ref.Kind == KindPlaylist && len(e.TrackList) >= embedTrackCap
	col.Complete = !col.Truncated
	return col, nil
}

// oembedTitle resolves ref's name via the oEmbed endpoint.
func (c *Catalog) oembedTitle(ctx context.Context, ref Ref) (string, error) {
	base := c.OEmbedBase
	if base == "" {
		base = defaultOEmbedBase
	}
	body, status, err := c.get(ctx, base+"?url="+url.QueryEscape(ref.URL()), maxOEmbedBody)
	if err != nil {
		return "", err
	}
	switch {
	case status == http.StatusNotFound:
		return "", fmt.Errorf("%w: %s", ErrNotFound, ref.URI())
	case status != http.StatusOK:
		return "", fmt.Errorf("%w %d", errOEmbedStatus, status)
	}
	var out struct {
		Title string `json:"title"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", fmt.Errorf("oembed: %w", err)
	}
	if strings.TrimSpace(out.Title) == "" {
		return "", errOEmbedNoTitle
	}
	return strings.TrimSpace(out.Title), nil
}

// get performs one GET, returning at most limit bytes of the body.
func (c *Catalog) get(ctx context.Context, endpoint string, limit int64) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, 0, fmt.Errorf("%w: %w", ErrCatalog, err)
	}
	ua := c.UserAgent
	if ua == "" {
		ua = defaultUserAgent
	}
	req.Header.Set("User-Agent", ua)
	req.Header.Set("Accept-Language", "en")

	client := c.Client
	if client == nil {
		client = &http.Client{Timeout: catalogTimeout}
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("%w: %w", ErrCatalog, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit))
	if err != nil {
		return nil, 0, fmt.Errorf("%w: read body: %w", ErrCatalog, err)
	}
	return body, resp.StatusCode, nil
}

func (c *Catalog) embedBase() string {
	if c.EmbedBase == "" {
		return defaultEmbedBase
	}
	return c.EmbedBase
}

// largestImage returns the URL of the widest image; images without a size
// rank below any sized one, in their listed order.
func largestImage(imgs []embedImage) string {
	best, bestW := "", -1
	for _, img := range imgs {
		if img.URL == "" {
			continue
		}
		w := 0
		switch {
		case img.Width != nil:
			w = *img.Width
		case img.MaxWidth != nil:
			w = *img.MaxWidth
		}
		if w > bestW {
			best, bestW = img.URL, w
		}
	}
	return best
}

func boolOr(b *bool, def bool) bool {
	if b == nil {
		return def
	}
	return *b
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v = strings.TrimSpace(v); v != "" {
			return v
		}
	}
	return ""
}
