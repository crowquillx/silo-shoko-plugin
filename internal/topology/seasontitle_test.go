package topology

import (
	"slices"
	"testing"

	"github.com/crowquillx/silo-shoko-plugin/internal/shoko"
)

// season is a series by its Shoko name, with its AniDB main title and air date
// when a test needs them.
func season(id int, name string, anidb ...string) shoko.Series {
	series := shoko.Series{IDs: shoko.IDs{ID: id}, Name: name, AniDB: &shoko.AnimeMetadata{}}
	if len(anidb) > 0 {
		series.AniDB.Titles = []shoko.Title{{Name: anidb[0], Language: "x-jat", Type: "Main"}}
	}
	if len(anidb) > 1 {
		series.AniDB.AirDate = anidb[1]
	}
	return series
}

func seasons(names ...string) []shoko.Series {
	members := make([]shoko.Series, len(names))
	for index, name := range names {
		members[index] = season(index+1, name)
	}
	return members
}

func TestSeasonTitlesDropTheShowTitle(t *testing.T) {
	tests := []struct {
		name string
		show string
		in   []string
		want []string
	}{
		{
			name: "subtitles after a colon",
			show: "Demon Slayer",
			in:   []string{"Demon Slayer", "Demon Slayer: Entertainment District Arc", "Demon Slayer: Mugen Train Arc"},
			want: []string{"Demon Slayer", "Entertainment District Arc", "Mugen Train Arc"},
		},
		{
			name: "case and punctuation differ, and one season has another name",
			show: "Armored Trooper VOTOMS",
			in:   []string{"Armor Hunter Mellowlink", "Armored Trooper VOTOMS", "Armored Trooper Votoms: Pailsen Files", "Armored Trooper VOTOMS: Case; Irvine"},
			want: []string{"Armor Hunter Mellowlink", "Armored Trooper VOTOMS", "Pailsen Files", "Case; Irvine"},
		},
		{
			name: "spacing and hyphens differ",
			show: "Uma Musume: Pretty Derby - Road to the Top",
			in:   []string{"Uma Musume: Pretty Derby - Road to the Top", "Umamusume: Pretty Derby - Beginning of a New Era"},
			want: []string{"Uma Musume: Pretty Derby - Road to the Top", "Beginning of a New Era"},
		},
		{
			name: "a year alone keeps the whole name",
			show: "Spy x Family",
			in:   []string{"Spy x Family", "Spy x Family (2023)", "Spy x Family Code: White", "Spy x Family 2025"},
			want: []string{"Spy x Family", "Spy x Family (2023)", "Code: White", "Spy x Family 2025"},
		},
		{
			name: "a note in parentheses alone keeps the whole name",
			show: "That Time I Got Reincarnated as a Slime",
			in:   []string{"That Time I Got Reincarnated as a Slime", "That Time I Got Reincarnated as a Slime (2021 Part 2)", "That Time I Got Reincarnated as a Slime the Movie: Scarlet Bond"},
			want: []string{"That Time I Got Reincarnated as a Slime", "That Time I Got Reincarnated as a Slime (2021 Part 2)", "The Movie: Scarlet Bond"},
		},
		{
			name: "a year after a subtitle stays with it",
			show: "Attack on Titan",
			in:   []string{"Attack on Titan", "Attack on Titan Season 3 (2019)", "Attack on Titan The Final Season"},
			want: []string{"Attack on Titan", "Season 3 (2019)", "The Final Season"},
		},
		{
			name: "numbers and a few characters keep the whole name",
			show: "Overlord",
			in:   []string{"Overlord", "Overlord II", "Overlord 2", "Overlord S", "Overlord 2nd", "Overlord OVA", "Overlord IV (2023)", "Overlord Next", "Overlord: The Sacred Kingdom"},
			want: []string{"Overlord", "Overlord II", "Overlord 2", "Overlord S", "Overlord 2nd", "Overlord OVA", "Overlord IV (2023)", "Next", "The Sacred Kingdom"},
		},
		{
			name: "punctuation alone keeps the whole name",
			show: "New Game!",
			in:   []string{"New Game!", "New Game!!"},
			want: []string{"New Game!", "New Game!!"},
		},
		{
			name: "the show title must end on a word",
			show: "Gate",
			in:   []string{"Gate", "Gatekeepers 21", "Gate-keepers", "Gate: Fire Dragon"},
			want: []string{"Gate", "Gatekeepers 21", "Gate-keepers", "Fire Dragon"},
		},
		{
			name: "a show title with a subtitle",
			show: "Ghost in the Shell: Stand Alone Complex",
			in:   []string{"Ghost in the Shell", "Ghost in the Shell: Arise", "Ghost in the Shell: Stand Alone Complex", "Ghost in the Shell: Stand Alone Complex 2nd GIG", "Ghost in the Shell: Stand Alone Complex - Solid State Society", "The Ghost in the Shell"},
			want: []string{"Ghost in the Shell", "Arise", "Ghost in the Shell: Stand Alone Complex", "2nd GIG", "Solid State Society", "The Ghost in the Shell"},
		},
		{
			name: "a show title with a year",
			show: "Fruits Basket (2019)",
			in:   []string{"Fruits Basket (2019)", "Fruits Basket Season 2", "Fruits Basket the Final Season"},
			want: []string{"Fruits Basket (2019)", "Season 2", "The Final Season"},
		},
		{
			name: "a show title with an exclamation mark",
			show: "Food Wars! Shokugeki no Soma",
			in:   []string{"Food Wars! Shokugeki no Soma", "Food Wars! The Second Plate"},
			want: []string{"Food Wars! Shokugeki no Soma", "The Second Plate"},
		},
		{
			name: "a stem no season needs is not stripped",
			show: "Frieren: Beyond Journey's End",
			in:   []string{"Frieren: Beyond Journey's End", "Frieren: Beyond Journey's End Season 2"},
			want: []string{"Frieren: Beyond Journey's End", "Season 2"},
		},
		{
			name: "quotes and trailing marks",
			show: "The Familiar of Zero",
			in:   []string{"The Familiar of Zero", `The Familiar of Zero: "Rondo" of Princesses`, "The Familiar of Zero… ~Next Shine~", "The Familiar of Zero x Bridge"},
			want: []string{"The Familiar of Zero", `"Rondo" of Princesses`, "Next Shine", "x Bridge"},
		},
		{
			name: "a group of several franchises keeps every name",
			show: "A Certain Magical Index",
			in:   []string{"A Certain Dark Item", "A Certain Magical Index", "A Certain Magical Index the Movie: The Miracle of Endymion", "A Certain Scientific Accelerator", "A Certain Scientific Railgun", "A Certain Scientific Railgun S"},
			want: []string{"A Certain Dark Item", "A Certain Magical Index", "A Certain Magical Index the Movie: The Miracle of Endymion", "A Certain Scientific Accelerator", "A Certain Scientific Railgun", "A Certain Scientific Railgun S"},
		},
		{
			name: "half the seasons having the show's name is enough",
			show: "Fire Force",
			in:   []string{"Fire Force", "Fire Force Season 3", "Soul Eater", "Soul Eater Not!"},
			want: []string{"Fire Force", "Season 3", "Soul Eater", "Soul Eater Not!"},
		},
		{
			name: "titles in another script",
			show: "鬼滅の刃",
			in:   []string{"鬼滅の刃", "鬼滅の刃 遊郭編"},
			want: []string{"鬼滅の刃", "鬼滅の刃 遊郭編"},
		},
		{
			name: "long subtitles in another script",
			show: "鬼滅の刃",
			in:   []string{"鬼滅の刃", "鬼滅の刃 刀鍛冶の里編"},
			want: []string{"鬼滅の刃", "刀鍛冶の里編"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := SeasonTitles(test.show, 0, seasons(test.in...)); !slices.Equal(got, test.want) {
				t.Errorf("titles = %q\n      want %q", got, test.want)
			}
		})
	}
}

func TestSeasonTitlesTellSameNamedSeasonsApart(t *testing.T) {
	tests := []struct {
		name    string
		show    string
		main    int
		members []shoko.Series
		want    []string
	}{
		{
			name: "the rest of the AniDB main title",
			show: "Tokyo Revengers",
			main: 1,
			members: []shoko.Series{
				season(1, "Tokyo Revengers", "Tokyo Revengers", "2021-04-11"),
				season(2, "Tokyo Revengers", "Tokyo Revengers: Seiya Kessen Hen", "2023-01-08"),
				season(3, "Tokyo Revengers", "Tokyo Revengers: Tenjiku Hen", "2023-10-04"),
			},
			want: []string{"Tokyo Revengers", "Seiya Kessen Hen", "Tenjiku Hen"},
		},
		{
			name: "a number or year keeps the show's name in front",
			show: "Gintama",
			main: 1,
			members: []shoko.Series{
				season(1, "Gintama", "Gintama", "2006-04-04"),
				season(2, "Gintama", "Gintama'", "2011-04-04"),
				season(3, "Gintama", "Gintama' (2012)", "2012-10-04"),
				season(4, "Gintama", "Gintama (2015)", "2015-04-08"),
				season(5, "Gintama", "Gintama IV", "2016-01-01"),
				season(6, "Gintama: The Movie", "Gekijouban Gintama", "2010-04-24"),
			},
			want: []string{"Gintama", "Gintama'", "Gintama' (2012)", "Gintama (2015)", "Gintama IV", "The Movie"},
		},
		{
			name: "the main series keeps the name wherever Shoko lists it",
			show: "Non Non Biyori",
			main: 3,
			members: []shoko.Series{
				season(1, "Non Non Biyori", "Non Non Biyori: Okinawa e Iku Koto ni Natta", "2014-07-23"),
				season(2, "Non Non Biyori Repeat", "Non Non Biyori Repeat", "2015-07-07"),
				season(3, "Non Non Biyori", "Non Non Biyori", "2013-10-08"),
			},
			want: []string{"Okinawa e Iku Koto ni Natta", "Repeat", "Non Non Biyori"},
		},
		{
			name: "without the main series, the one that aired first keeps the name",
			show: "A Certain Magical Index",
			main: 9,
			members: []shoko.Series{
				season(1, "A Certain Scientific Railgun", "Toaru Kagaku no Railgun: Motto Marutto Railgun", "2010-07-30"),
				season(2, "A Certain Scientific Railgun", "Toaru Kagaku no Railgun", "2009-10-03"),
				season(9, "A Certain Magical Index", "Toaru Majutsu no Index", "2008-10-05"),
				season(4, "A Certain Dark Item", "Toaru Anbu no Item", "2026-01-01"),
				season(5, "A Certain Scientific Accelerator", "Toaru Kagaku no Accelerator", "2019-07-12"),
			},
			want: []string{"A Certain Scientific Railgun: Motto Marutto Railgun", "A Certain Scientific Railgun", "A Certain Magical Index", "A Certain Dark Item", "A Certain Scientific Accelerator"},
		},
		{
			name: "names that differ only in case are the same name",
			show: "My-OTOME",
			main: 1,
			members: []shoko.Series{
				season(1, "My-OTOME", "Mai-Otome", "2005-10-06"),
				season(2, "My-Otome", "Mai-Otome 0: S.ifr", "2008-02-22"),
			},
			want: []string{"My-OTOME", "0: S.ifr"},
		},
		{
			name: "names that differ in punctuation are different names",
			show: "Cells at Work!",
			main: 1,
			members: []shoko.Series{
				season(1, "Cells at Work!", "Hataraku Saibou", "2018-07-08"),
				season(2, "Cells at Work!!", "Hataraku Saibou!!", "2021-01-09"),
			},
			want: []string{"Cells at Work!", "Cells at Work!!"},
		},
		{
			name: "an unrelated or missing AniDB title leaves the name",
			show: "Medalist",
			main: 1,
			members: []shoko.Series{
				season(1, "Medalist", "Medalist", "2025-01-05"),
				season(2, "Medalist", "Something Else", "2026-01-25"),
				{IDs: shoko.IDs{ID: 3}, Name: "Medalist"},
			},
			want: []string{"Medalist", "Medalist", "Medalist"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := SeasonTitles(test.show, test.main, test.members); !slices.Equal(got, test.want) {
				t.Errorf("titles = %q\n      want %q", got, test.want)
			}
		})
	}
}
