package identity

import "testing"

func TestParse(t *testing.T) {
	token := Parse("Show/S01E02 [Shoko Group=12] [Shoko Series=7] [Shoko Episode=42] [Shoko File=99].mkv")
	if token.GroupID != 12 || token.SeriesID != 7 || token.EpisodeID != 42 || token.FileID != 99 {
		t.Fatalf("Parse() = %#v", token)
	}
	if !token.Valid() {
		t.Fatal("token should be valid")
	}
	ids := token.ProviderIDs()
	if ids["shoko_group"] != "12" || ids["shoko_series"] != "7" || ids["shoko_episode"] != "42" || ids["shoko_file"] != "99" {
		t.Fatalf("ProviderIDs() = %#v", ids)
	}
}

func TestParseIgnoresInvalidIDs(t *testing.T) {
	token := Parse("[Shoko Series=0] [Shoko Episode=nope]")
	if token.Valid() {
		t.Fatalf("invalid token considered valid: %#v", token)
	}
}
