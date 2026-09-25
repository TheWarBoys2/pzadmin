package steam

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestParseIDsFromAnythingPasted(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"https://steamcommunity.com/sharedfiles/filedetails/?id=2392709985", []string{"2392709985"}},
		{"https://steamcommunity.com/workshop/filedetails/?id=2392709985", []string{"2392709985"}},
		{"steamcommunity.com/sharedfiles/filedetails/?id=2392709985&searchtext=hair", []string{"2392709985"}},
		{"2392709985", []string{"2392709985"}},
		{"2392709985;2169435993", []string{"2392709985", "2169435993"}},
		{"2392709985\n2169435993\n", []string{"2392709985", "2169435993"}},
		// A duplicate in a pasted list should collapse.
		{"2392709985 2392709985", []string{"2392709985"}},
		{"nothing here", nil},
		{"", nil},
	}
	for _, tc := range cases {
		got := ParseIDs(tc.in)
		if len(got) != len(tc.want) {
			t.Fatalf("ParseIDs(%q) = %v, want %v", tc.in, got, tc.want)
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Fatalf("ParseIDs(%q) = %v, want %v", tc.in, got, tc.want)
			}
		}
	}
}

// A realistic Project Zomboid Workshop description, which is where the mod ID
// actually lives.
const description = `Adds a load of new hairstyles.

[b]Features[/b]
- lots of hair

Workshop ID: 2392709985
Mod ID: FancyHair
Mod ID: FancyHairAddon
Map Folder: MuldraughKY`

func fakeSteam(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/ISteamRemoteStorage/GetPublishedFileDetails/v1/", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		w.Header().Set("Content-Type", "application/json")
		if r.Form.Get("publishedfileids[0]") == "999" {
			w.Write([]byte(`{"response":{"result":1,"publishedfiledetails":[
			  {"publishedfileid":"999","result":9}]}}`))
			return
		}
		w.Write([]byte(`{"response":{"result":1,"publishedfiledetails":[
		  {"publishedfileid":"2392709985","result":1,"title":"Fancy Hair",
		   "description":` + jsonQuote(description) + `,
		   "time_updated":1700000000,"file_size":"1048576","consumer_app_id":108600}]}}`))
	})
	mux.HandleFunc("/ISteamRemoteStorage/GetCollectionDetails/v1/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"response":{"collectiondetails":[
		  {"publishedfileid":"123","result":1,"children":[
		    {"publishedfileid":"111","filetype":0},
		    {"publishedfileid":"222","filetype":0}]}]}}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func jsonQuote(s string) string {
	out := []byte{'"'}
	for _, r := range s {
		switch r {
		case '"':
			out = append(out, '\\', '"')
		case '\n':
			out = append(out, '\\', 'n')
		case '\\':
			out = append(out, '\\', '\\')
		default:
			out = append(out, string(r)...)
		}
	}
	return string(append(out, '"'))
}

func TestDetailsExtractsModIDsFromTheDescription(t *testing.T) {
	srv := fakeSteam(t)
	items, err := New(srv.URL).Details(context.Background(), []string{"2392709985"})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("expected one item, got %d", len(items))
	}
	item := items[0]
	if item.Title != "Fancy Hair" || item.Missing {
		t.Fatalf("unexpected item %#v", item)
	}
	if len(item.ModIDs) != 2 || item.ModIDs[0] != "FancyHair" || item.ModIDs[1] != "FancyHairAddon" {
		t.Fatalf("mod IDs not read from the description: %#v", item.ModIDs)
	}
	if len(item.MapFolders) != 1 || item.MapFolders[0] != "MuldraughKY" {
		t.Fatalf("map folder not read: %#v", item.MapFolders)
	}
	if item.AppID != ProjectZomboidAppID {
		t.Fatalf("app id wrong: %d", item.AppID)
	}
	if item.SizeBytes != 1048576 {
		t.Fatalf("file_size as a string should still parse: %d", item.SizeBytes)
	}
	if item.UpdatedAt.IsZero() {
		t.Fatal("update time missing")
	}
}

// A deleted mod in a pasted list must not sink the rest of the request.
func TestDeletedItemComesBackAsMissing(t *testing.T) {
	srv := fakeSteam(t)
	items, err := New(srv.URL).Details(context.Background(), []string{"999"})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || !items[0].Missing {
		t.Fatalf("expected a missing item, got %#v", items)
	}
}

func TestCollectionExpands(t *testing.T) {
	srv := fakeSteam(t)
	ids, err := New(srv.URL).Collection(context.Background(), "123")
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 2 || ids[0] != "111" || ids[1] != "222" {
		t.Fatalf("unexpected children %v", ids)
	}
}

func TestUnreachableSteamIsAPlainError(t *testing.T) {
	// Nothing listening: the message has to say Steam could not be reached
	// rather than leaking a dial error to the operator.
	_, err := New("http://127.0.0.1:1").Details(context.Background(), []string{"1"})
	if err == nil {
		t.Fatal("expected an error")
	}
	if got := err.Error(); got[:6] != "steam:" {
		t.Fatalf("unexpected message %q", got)
	}
}

func TestEmptyRequestDoesNothing(t *testing.T) {
	items, err := New("http://127.0.0.1:1").Details(context.Background(), nil)
	if err != nil || items != nil {
		t.Fatalf("an empty request should be a no-op, got %v %v", items, err)
	}
}

// Every one of these is a shape that appears on real Project Zomboid Workshop
// pages. The mod ID is the only thing a Workshop page tells you that belongs in
// the Mods= line, so failing to read it means falling back to guesswork.
func TestModIDIsReadFromEveryCommonDescriptionShape(t *testing.T) {
	cases := []struct {
		name string
		text string
		want []string
	}{
		{"plain at the bottom",
			"The night glow is a fullbright overlay.\n\nWorkshop ID: 3755993986\nMod ID: GasPumpIndicator",
			[]string{"GasPumpIndicator"}},
		{"bold with the colon inside the tag",
			"stuff\n[b]Mod ID:[/b] FancyHair",
			[]string{"FancyHair"}},
		{"bold with the colon outside",
			"stuff\n[b]Mod ID[/b]: FancyHair",
			[]string{"FancyHair"}},
		{"heading markup around it",
			"[h1]Details[/h1]\n[b]Mod ID:[/b] FancyHair",
			[]string{"FancyHair"}},
		{"no space after the colon",
			"Mod ID:FancyHair",
			[]string{"FancyHair"}},
		{"plural with a list",
			"Mod IDs: FancyHair, FancyHairAddon",
			[]string{"FancyHair", "FancyHairAddon"}},
		{"several lines",
			"Mod ID: One\nMod ID: Two",
			[]string{"One", "Two"}},
		{"trailing prose is ignored",
			"Mod ID: FancyHair (requires Tsarslib)",
			[]string{"FancyHair"}},
		{"lower case",
			"mod id: fancyhair",
			[]string{"fancyhair"}},
		{"repeated ids collapse",
			"Mod ID: FancyHair\n[b]Mod ID:[/b] FancyHair",
			[]string{"FancyHair"}},
		{"no mod id at all",
			"Just a description with no ID anywhere.",
			nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := uniqueMatches(reModID, stripBBCode(tc.text))
			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("got %v, want %v", got, tc.want)
				}
			}
		})
	}
}

func TestMapFolderIsReadTheSameWay(t *testing.T) {
	got := uniqueMatches(reMapID, stripBBCode("[b]Map Folder:[/b] MuldraughKY, RiversideKY"))
	if len(got) != 2 || got[0] != "MuldraughKY" || got[1] != "RiversideKY" {
		t.Fatalf("got %v", got)
	}
}

func TestStripBBCodeLeavesTheTextAlone(t *testing.T) {
	in := "[h1]Title[/h1]\n[b]Bold[/b] and [url=http://x]link[/url]\nMod ID: Foo"
	out := stripBBCode(in)
	if !strings.Contains(out, "Mod ID: Foo") || !strings.Contains(out, "Bold and") {
		t.Fatalf("stripping removed too much: %q", out)
	}
	if strings.Contains(out, "[b]") || strings.Contains(out, "[/url]") {
		t.Fatalf("markup survived: %q", out)
	}
}
