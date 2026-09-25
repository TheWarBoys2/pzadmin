// Package steam resolves Steam Workshop items and collections.
//
// Both endpoints used here are public and take no API key: they are the same
// ones the game and every Workshop tool use. That matters, because it means
// adding a mod by pasting a link needs no setup at all.
//
// A Workshop item does not directly tell you the mod ID Project Zomboid wants
// in its Mods= line — that lives in the mod's own mod.info, which only exists
// once the server has downloaded it. Project Zomboid mod authors conventionally
// list it in the item description ("Mod ID: Foo"), so that is read where
// present; where it is absent, PZAdmin adds the Workshop ID, lets the server
// download the mod, and picks the real ID up off disk afterwards.
package steam

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Item is one resolved Workshop entry.
type Item struct {
	WorkshopID string `json:"workshopId"`
	Title      string `json:"title"`
	// ModIDs are the Project Zomboid mod IDs declared in the description.
	ModIDs []string `json:"modIds"`
	// MapFolders are map names declared in the description, which belong in
	// the Map= line rather than Mods=.
	MapFolders []string  `json:"mapFolders,omitempty"`
	UpdatedAt  time.Time `json:"updatedAt,omitempty"`
	SizeBytes  int64     `json:"sizeBytes,omitempty"`
	// Missing is set when Steam has no such item, usually a deleted mod.
	Missing bool `json:"missing"`
	// AppID lets the caller notice a link to something that is not a Project
	// Zomboid mod at all.
	AppID int `json:"appId,omitempty"`
}

// Client talks to the Steam Web API.
type Client struct {
	http *http.Client
	base string
}

// New returns a client. baseURL is for tests; empty uses the public API.
func New(baseURL string) *Client {
	if baseURL == "" {
		baseURL = "https://api.steampowered.com"
	}
	return &Client{
		http: &http.Client{Timeout: 20 * time.Second},
		base: strings.TrimSuffix(baseURL, "/"),
	}
}

// ProjectZomboidAppID is the app Workshop items must belong to.
const ProjectZomboidAppID = 108600

var (
	reWorkshopID = regexp.MustCompile(`(?:^|[?&/])(?:id=)?(\d{6,12})(?:$|[&/#\s])`)
	reBareID     = regexp.MustCompile(`^\d{6,12}$`)
	// Mod authors put these at the bottom of the Workshop description, which is
	// the only place a Workshop page states the ID that belongs in Mods=.
	// Matching happens after BBCode is stripped, so "[b]Mod ID:[/b] Foo" and
	// "Mod ID: Foo" are the same thing by the time we get here.
	reModID = regexp.MustCompile(`(?im)^\s*Mod\s*ID[s]?\s*[:=]\s*([^\r\n]+)`)
	reMapID = regexp.MustCompile(`(?im)^\s*Map\s*Folder[s]?\s*[:=]\s*([^\r\n]+)`)
	// A mod ID is a bare identifier. Capturing the rest of the line and then
	// taking the leading identifier from each comma-separated part copes with
	// both "Mod IDs: A, B" and "Mod ID: A (requires B)".
	reIdentifier = regexp.MustCompile(`^[A-Za-z0-9_.\-]+`)
	reBBCode     = regexp.MustCompile(`\[/?[A-Za-z0-9=*"'\s._-]{1,40}\]`)
)

// ParseIDs pulls every Workshop ID out of arbitrary pasted text: links, bare
// IDs, or a whole list of either.
func ParseIDs(text string) []string {
	seen := map[string]bool{}
	var out []string
	add := func(id string) {
		if id == "" || seen[id] {
			return
		}
		seen[id] = true
		out = append(out, id)
	}
	for _, field := range strings.FieldsFunc(text, func(r rune) bool {
		return r == '\n' || r == '\r' || r == ',' || r == ';' || r == ' ' || r == '\t'
	}) {
		field = strings.TrimSpace(field)
		if field == "" {
			continue
		}
		if reBareID.MatchString(field) {
			add(field)
			continue
		}
		if m := reWorkshopID.FindStringSubmatch(field + "/"); m != nil {
			add(m[1])
		}
	}
	return out
}

// Details resolves Workshop items. Unknown IDs come back with Missing set
// rather than as an error, so one dead mod in a paste does not sink the rest.
func (c *Client) Details(ctx context.Context, ids []string) ([]Item, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	if len(ids) > 200 {
		ids = ids[:200]
	}

	form := url.Values{}
	form.Set("itemcount", strconv.Itoa(len(ids)))
	for i, id := range ids {
		form.Set(fmt.Sprintf("publishedfileids[%d]", i), id)
	}

	var payload struct {
		Response struct {
			Result               int `json:"result"`
			PublishedFileDetails []struct {
				PublishedFileID string `json:"publishedfileid"`
				Result          int    `json:"result"`
				Title           string `json:"title"`
				Description     string `json:"description"`
				TimeUpdated     int64  `json:"time_updated"`
				FileSize        any    `json:"file_size"`
				ConsumerAppID   int    `json:"consumer_app_id"`
				CreatorAppID    int    `json:"creator_app_id"`
			} `json:"publishedfiledetails"`
		} `json:"response"`
	}
	if err := c.post(ctx, "/ISteamRemoteStorage/GetPublishedFileDetails/v1/", form, &payload); err != nil {
		return nil, err
	}

	out := make([]Item, 0, len(payload.Response.PublishedFileDetails))
	for _, d := range payload.Response.PublishedFileDetails {
		item := Item{
			WorkshopID: d.PublishedFileID,
			Title:      strings.TrimSpace(d.Title),
			AppID:      firstNonZero(d.ConsumerAppID, d.CreatorAppID),
		}
		// Steam uses result 1 for success; anything else means it could not
		// return the item, most often because it has been deleted.
		if d.Result != 1 || item.Title == "" {
			item.Missing = true
			out = append(out, item)
			continue
		}
		if d.TimeUpdated > 0 {
			item.UpdatedAt = time.Unix(d.TimeUpdated, 0).UTC()
		}
		item.SizeBytes = parseSize(d.FileSize)
		plain := stripBBCode(d.Description)
		item.ModIDs = uniqueMatches(reModID, plain)
		item.MapFolders = uniqueMatches(reMapID, plain)
		out = append(out, item)
	}
	return out, nil
}

// Collection expands a Workshop collection into the items it contains.
func (c *Client) Collection(ctx context.Context, id string) ([]string, error) {
	form := url.Values{}
	form.Set("collectioncount", "1")
	form.Set("publishedfileids[0]", id)

	var payload struct {
		Response struct {
			CollectionDetails []struct {
				PublishedFileID string `json:"publishedfileid"`
				Result          int    `json:"result"`
				Children        []struct {
					PublishedFileID string `json:"publishedfileid"`
					FileType        int    `json:"filetype"`
				} `json:"children"`
			} `json:"collectiondetails"`
		} `json:"response"`
	}
	if err := c.post(ctx, "/ISteamRemoteStorage/GetCollectionDetails/v1/", form, &payload); err != nil {
		return nil, err
	}
	for _, d := range payload.Response.CollectionDetails {
		if d.Result != 1 {
			continue
		}
		out := make([]string, 0, len(d.Children))
		for _, child := range d.Children {
			out = append(out, child.PublishedFileID)
		}
		return out, nil
	}
	return nil, nil
}

func (c *Client) post(ctx context.Context, path string, form url.Values, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+path,
		strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("steam: could not be reached: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("steam: returned %s", resp.Status)
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("steam: unexpected response: %w", err)
	}
	return nil
}

// parseSize copes with file_size arriving as either a number or a string,
// which Steam has done both of.
func parseSize(v any) int64 {
	switch t := v.(type) {
	case float64:
		return int64(t)
	case string:
		n, _ := strconv.ParseInt(t, 10, 64)
		return n
	}
	return 0
}

// stripBBCode removes Steam's markup so the patterns only have to cope with
// the text an author actually wrote.
func stripBBCode(text string) string {
	return reBBCode.ReplaceAllString(text, "")
}

func uniqueMatches(re *regexp.Regexp, text string) []string {
	seen := map[string]bool{}
	var out []string
	for _, m := range re.FindAllStringSubmatch(text, -1) {
		// Split only on separators an author would use for a list. Splitting
		// on spaces too would turn "Foo (requires Bar)" into three entries.
		for _, part := range strings.FieldsFunc(m[1], func(r rune) bool {
			return r == ',' || r == ';' || r == '|'
		}) {
			id := reIdentifier.FindString(strings.TrimSpace(part))
			if id == "" || seen[id] {
				continue
			}
			// A trailing full stop is sentence punctuation, not part of an ID.
			id = strings.TrimRight(id, ".")
			if id == "" || seen[id] {
				continue
			}
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}

func firstNonZero(values ...int) int {
	for _, v := range values {
		if v != 0 {
			return v
		}
	}
	return 0
}
