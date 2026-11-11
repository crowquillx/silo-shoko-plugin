// Package live listens to Shoko Server's SignalR events and turns them into
// batches of work: files, series and groups to re-read into the VFS, and
// series, episodes and groups whose metadata or artwork Silo should refresh.
package live

import (
	"encoding/json"
	"strconv"
	"strings"

	"github.com/crowquillx/silo-shoko-plugin/internal/reconcile"
)

// HubPath and Feeds name Shoko's aggregate hub and the feeds it pushes. Daily
// (6.x) servers use the metadata, file, release and group feeds; stable (5.x)
// servers send everything on the shoko feed. A server skips feeds it does not
// have.
const (
	HubPath = "/signalr/aggregate"
	Feeds   = "shoko,metadata,file,release,group"
)

// Batch is the work that a set of events calls for.
type Batch struct {
	// VFS is what to re-read from Shoko before reconciling the VFS.
	VFS reconcile.Changes
	// Recrawl asks for a full crawl, because Shoko changed too much at once
	// to name, such as recreating every group.
	Recrawl bool
	// Refresh is what Silo should refresh the metadata or artwork of.
	Refresh Refresh
}

// Refresh names the Shoko entries whose Silo shows need refreshing.
type Refresh struct {
	Series   []int
	Episodes []int
	Groups   []int
}

// Empty reports whether nothing needs refreshing.
func (r Refresh) Empty() bool {
	return len(r.Series) == 0 && len(r.Episodes) == 0 && len(r.Groups) == 0
}

// Empty reports whether the batch has no work.
func (b Batch) Empty() bool {
	return b.VFS.Empty() && !b.Recrawl && b.Refresh.Empty()
}

// Merge adds other's work to b, without duplicates.
func (b *Batch) Merge(other Batch) {
	b.VFS.Merge(other.VFS)
	b.Recrawl = b.Recrawl || other.Recrawl
	b.Refresh.Series = appendAll(b.Refresh.Series, other.Refresh.Series...)
	b.Refresh.Episodes = appendAll(b.Refresh.Episodes, other.Refresh.Episodes...)
	b.Refresh.Groups = appendAll(b.Refresh.Groups, other.Refresh.Groups...)
}

// Events about one file, all with its FileID: matched to episodes or not,
// unmatched, moved, renamed, deleted, or marked ignored or a variation.
// Daily servers name them by feed; stable servers prefix ShokoEvent.
var fileEvents = map[string]bool{
	"release:saved":   true,
	"release:removed": true,
	// release:removed was called release:deleted on earlier daily servers.
	"release:deleted":           true,
	"file:deleted":              true,
	"file:relocated":            true,
	"file:updated":              true,
	"ShokoEvent:FileMatched":    true,
	"ShokoEvent:FileNotMatched": true,
	"ShokoEvent:FileDeleted":    true,
	"ShokoEvent:FileMoved":      true,
	"ShokoEvent:FileRenamed":    true,
}

// Daily servers name metadata events after their reason, such as
// metadata:series.imageupdated, so they are matched by prefix. Stable servers
// send one event per kind with the reason in it.
var metadataPrefixes = []string{"metadata:series.", "metadata:episode.", "metadata:movie."}

var stableMetadataEvents = map[string]bool{
	"ShokoEvent:SeriesUpdated":  true,
	"ShokoEvent:EpisodeUpdated": true,
	"ShokoEvent:MovieUpdated":   true,
}

// Metadata events with these suffixes are not about a series' metadata.
var otherMetadataSuffixes = map[string]bool{
	"customTagsChanged": true,
	"hiddenChanged":     true,
}

type fileEvent struct {
	FileID *int `json:"FileID"`
}

type metadataEvent struct {
	Reason          json.RawMessage `json:"Reason"`
	ShokoSeriesIDs  []int           `json:"ShokoSeriesIDs"`
	ShokoEpisodeIDs []int           `json:"ShokoEpisodeIDs"`
}

type groupEvent struct {
	Reason  json.RawMessage `json:"Reason"`
	GroupID int             `json:"GroupID"`
}

type seriesMovedEvent struct {
	SeriesID   int `json:"SeriesID"`
	OldGroupID int `json:"OldGroupID"`
	NewGroupID int `json:"NewGroupID"`
}

type linksChangedEvent struct {
	ShokoSeriesIDs []int `json:"ShokoSeriesIDs"`
}

// Translate returns the work an event calls for, and false for an event that
// calls for none.
func Translate(target string, arguments []json.RawMessage) (Batch, bool) {
	if target == "group:recreated" {
		return Batch{Recrawl: true}, true
	}
	if len(arguments) == 0 {
		return Batch{}, false
	}
	argument := arguments[0]
	var batch Batch
	switch {
	case fileEvents[target]:
		var event fileEvent
		if json.Unmarshal(argument, &event) != nil || event.FileID == nil || *event.FileID <= 0 {
			return Batch{}, false
		}
		batch.VFS.Files = []int{*event.FileID}

	case stableMetadataEvents[target] || isDailyMetadataEvent(target):
		var event metadataEvent
		if json.Unmarshal(argument, &event) != nil {
			return Batch{}, false
		}
		reason := parseReason(event.Reason, target)
		for _, id := range positive(event.ShokoSeriesIDs) {
			// Artwork never moves a file in the VFS.
			if reason.touchesMetadata() {
				batch.VFS.Series = appendAll(batch.VFS.Series, id)
			}
			// A removed entry leaves Silo with the VFS change.
			if !reason.removedOnly() {
				batch.Refresh.Series = appendAll(batch.Refresh.Series, id)
			}
		}
		if !reason.removedOnly() {
			batch.Refresh.Episodes = appendAll(batch.Refresh.Episodes, positive(event.ShokoEpisodeIDs)...)
		}

	case target == "metadata:links.changed":
		// A changed provider link changes the IDs and titles Silo shows, but
		// not the VFS, which follows AniDB.
		var event linksChangedEvent
		if json.Unmarshal(argument, &event) != nil {
			return Batch{}, false
		}
		batch.Refresh.Series = positive(event.ShokoSeriesIDs)

	case target == "group:series.moved":
		var event seriesMovedEvent
		if json.Unmarshal(argument, &event) != nil || event.SeriesID <= 0 {
			return Batch{}, false
		}
		batch.VFS.Series = []int{event.SeriesID}
		batch.VFS.Groups = positive([]int{event.OldGroupID, event.NewGroupID})
		batch.Refresh.Groups = append([]int(nil), batch.VFS.Groups...)

	case strings.HasPrefix(target, "group:group."):
		// A renamed group renames its show folder; a group's artwork is the
		// show's.
		var event groupEvent
		if json.Unmarshal(argument, &event) != nil || event.GroupID <= 0 {
			return Batch{}, false
		}
		reason := parseReason(event.Reason, target)
		if reason.touchesMetadata() {
			batch.VFS.Groups = []int{event.GroupID}
		}
		if !reason.removedOnly() {
			batch.Refresh.Groups = []int{event.GroupID}
		}
	}
	return batch, !batch.Empty()
}

func isDailyMetadataEvent(target string) bool {
	for _, prefix := range metadataPrefixes {
		if suffix, ok := strings.CutPrefix(target, prefix); ok {
			return !otherMetadataSuffixes[suffix]
		}
	}
	return false
}

// reason is Shoko's UpdateReason, a set of flags.
type reason uint8

const (
	reasonAdded        reason = 1
	reasonUpdated      reason = 2
	reasonRemoved      reason = 4
	reasonImageAdded   reason = 8
	reasonImageUpdated reason = 16
	reasonImageRemoved reason = 32

	reasonMetadata = reasonAdded | reasonUpdated | reasonRemoved
)

var reasonNames = map[string]reason{
	"added":           reasonAdded,
	"metadataadded":   reasonAdded,
	"updated":         reasonUpdated,
	"metadataupdated": reasonUpdated,
	"removed":         reasonRemoved,
	"metadataremoved": reasonRemoved,
	"imageadded":      reasonImageAdded,
	"imageupdated":    reasonImageUpdated,
	"imageremoved":    reasonImageRemoved,
}

// parseReason reads the reason Shoko sends: a name, a comma-separated list of
// names, or for a combination with no name of its own, the flags as a number.
// Without one, the reason in a daily event's name is used, and failing that
// the event counts as a plain update.
func parseReason(raw json.RawMessage, target string) reason {
	var text string
	var number int
	switch {
	case json.Unmarshal(raw, &text) == nil:
		if value, ok := reasonFromText(text); ok {
			return value
		}
	case json.Unmarshal(raw, &number) == nil:
		if number > 0 && number < 64 {
			return reason(number)
		}
	}
	if dot := strings.LastIndexByte(target, '.'); dot >= 0 {
		if value, ok := reasonFromText(target[dot+1:]); ok {
			return value
		}
	}
	return reasonUpdated
}

func reasonFromText(text string) (reason, bool) {
	if number, err := strconv.Atoi(strings.TrimSpace(text)); err == nil && number > 0 && number < 64 {
		return reason(number), true
	}
	var value reason
	for _, part := range strings.Split(text, ",") {
		flag, ok := reasonNames[strings.ToLower(strings.TrimSpace(part))]
		if !ok {
			return 0, false
		}
		value |= flag
	}
	return value, value != 0
}

// touchesMetadata reports whether more than artwork changed.
func (r reason) touchesMetadata() bool { return r&reasonMetadata != 0 }

// removedOnly reports whether the entry was removed and nothing else.
func (r reason) removedOnly() bool { return r == reasonRemoved }

func positive(ids []int) []int {
	result := make([]int, 0, len(ids))
	for _, id := range ids {
		if id > 0 {
			result = appendAll(result, id)
		}
	}
	return result
}

func appendAll(values []int, more ...int) []int {
	for _, value := range more {
		found := false
		for _, existing := range values {
			if existing == value {
				found = true
				break
			}
		}
		if !found {
			values = append(values, value)
		}
	}
	return values
}
