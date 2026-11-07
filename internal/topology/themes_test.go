package topology

import (
	"path/filepath"
	"reflect"
	"testing"
)

func themeEntry(logical, target string, seriesID, season int) Entry {
	return Entry{LogicalPath: logical, TargetPath: target, SourceRoot: "/m", ShokoSeriesID: seriesID, SeasonNumber: season}
}

func TestThemePairs(t *testing.T) {
	tests := []struct {
		name    string
		entries []Entry
		want    []ThemePair
	}{
		{
			name: "flat show folder is the show",
			entries: []Entry{
				themeEntry("Frieren (2023)/Season 01/a.mkv", "/m/Frieren/01.mkv", 1, 1),
				themeEntry("Frieren (2023)/Season 00/b.mkv", "/m/Frieren/sp1.mkv", 1, 0),
			},
			want: []ThemePair{{VFSDir: "Frieren (2023)", SourceDir: "/m/Frieren"}},
		},
		{
			name: "season folder layout",
			entries: []Entry{
				themeEntry("Show (2020)/Season 01/a.mkv", "/m/Show/Season 1/01.mkv", 1, 1),
				themeEntry("Show (2020)/Season 01/b.mkv", "/m/Show/Season 1/02.mkv", 1, 1),
			},
			want: []ThemePair{{VFSDir: "Show (2020)", SourceDir: "/m/Show"}, {VFSDir: "Show (2020)/Season 01", SourceDir: "/m/Show/Season 1"}},
		},
		{
			name: "group of series in their own folders",
			entries: []Entry{
				themeEntry("Mono (2009)/Season 01/a.mkv", "/m/Mono/Bake/01.mkv", 1, 1),
				themeEntry("Mono (2009)/Season 02/a.mkv", "/m/Mono/Nise/01.mkv", 2, 2),
			},
			want: []ThemePair{{VFSDir: "Mono (2009)", SourceDir: "/m/Mono"}, {VFSDir: "Mono (2009)/Season 01", SourceDir: "/m/Mono/Bake"}, {VFSDir: "Mono (2009)/Season 02", SourceDir: "/m/Mono/Nise"}},
		},
		{
			name:    "files in the managed root are never paired",
			entries: []Entry{themeEntry("Show (2020)/Season 01/a.mkv", "/m/01.mkv", 1, 1)},
		},
		{
			name: "a folder shared with another show is never paired",
			entries: []Entry{
				themeEntry("One (2020)/Season 01/a.mkv", "/m/downloads/one-01.mkv", 1, 1),
				themeEntry("Two (2021)/Season 01/a.mkv", "/m/downloads/two-01.mkv", 2, 1),
			},
		},
		{
			name: "a series spread over two folders pairs only the show",
			entries: []Entry{
				themeEntry("Show (2020)/Season 01/a.mkv", "/m/Show/Part A/01.mkv", 1, 1),
				themeEntry("Show (2020)/Season 01/b.mkv", "/m/Show/Part B/13.mkv", 1, 1),
			},
			want: []ThemePair{{VFSDir: "Show (2020)", SourceDir: "/m/Show"}},
		},
		{
			name: "two series sharing one folder pair only the show",
			entries: []Entry{
				themeEntry("Mono (2009)/Season 01/a.mkv", "/m/Mono/01.mkv", 1, 1),
				themeEntry("Mono (2009)/Season 02/a.mkv", "/m/Mono/02.mkv", 2, 2),
			},
			want: []ThemePair{{VFSDir: "Mono (2009)", SourceDir: "/m/Mono"}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for i := range tt.want {
				tt.want[i].SourceDir = filepath.FromSlash(tt.want[i].SourceDir)
			}
			if got := ThemePairs(tt.entries); !reflect.DeepEqual(got, tt.want) && !(len(got) == 0 && len(tt.want) == 0) {
				t.Fatalf("pairs = %#v\nwant %#v", got, tt.want)
			}
		})
	}
}

func TestThemePairsNeverSpanManagedFolders(t *testing.T) {
	entries := []Entry{
		{LogicalPath: "Show (2020)/Season 01/a.mkv", TargetPath: "/a/Show/01.mkv", SourceRoot: "/a", ShokoSeriesID: 1, SeasonNumber: 1},
		{LogicalPath: "Show (2020)/Season 02/a.mkv", TargetPath: "/b/Show/01.mkv", SourceRoot: "/b", ShokoSeriesID: 2, SeasonNumber: 2},
	}
	for _, pair := range ThemePairs(entries) {
		if pair.VFSDir == "Show (2020)" {
			t.Fatalf("show paired across managed folders: %#v", pair)
		}
	}
}
