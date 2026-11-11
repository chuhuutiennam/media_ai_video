package result

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/url"
	"sort"
	"strings"
	"sync"

	"go.uber.org/zap"
	"media-ai-gateway/internal/pipeline"
)

// Keyword is the complete MF keyword request body. Source metadata stays in logs
// and the idempotency header, never in the public JSON contract.
type Keyword struct {
	TopicLabel string `json:"topic_label" yaml:"topic_label"`
	TopicInfo  string `json:"topic_info" yaml:"topic_info"`
}

type GestureKeywordConfig struct {
	Mode             string             `yaml:"mode"` // legacy | keyword
	BaseURL          string             `yaml:"base_url"`
	Mapping          map[string]Keyword `yaml:"mapping"`
	CacheSize        int                `yaml:"cache_size"`
	OpenPalmMaxGapMs int64              `yaml:"open_palm_max_gap_ms"`
	OpenPalmHoldMs   int64              `yaml:"open_palm_hold_ms"`
	OpenPalmMinScore float64            `yaml:"open_palm_min_score"`
}

func DefaultGestureKeywordConfig() GestureKeywordConfig {
	return GestureKeywordConfig{
		Mode: "legacy", CacheSize: 4096, OpenPalmMaxGapMs: 500,
		OpenPalmHoldMs: 200, OpenPalmMinScore: 0.6,
		Mapping: map[string]Keyword{
			"Closed_Fist": {"EMOTION", "determination"},
			"Open_Palm":   {"EMOTION", "hello"},
			"Pointing_Up": {"EMOTION", "attention"},
			"Thumb_Down":  {"EMOTION", "disapprove"},
			"Thumb_Up":    {"EMOTION", "approve"},
			"Victory":     {"EMOTION", "celebrate"},
			"ILoveYou":    {"EMOTION", "love"},
			"Wave":        {"EMOTION", "congrat"},
		},
	}
}

// WithDefaults also copies the map: callback sinks own immutable configuration.
func (c GestureKeywordConfig) WithDefaults() GestureKeywordConfig {
	d := DefaultGestureKeywordConfig()
	if c.Mode != "" {
		d.Mode = c.Mode
	}
	d.BaseURL = c.BaseURL
	if c.CacheSize != 0 {
		d.CacheSize = c.CacheSize
	}
	if c.OpenPalmMaxGapMs != 0 {
		d.OpenPalmMaxGapMs = c.OpenPalmMaxGapMs
	}
	if c.OpenPalmHoldMs != 0 {
		d.OpenPalmHoldMs = c.OpenPalmHoldMs
	}
	if c.OpenPalmMinScore != 0 {
		d.OpenPalmMinScore = c.OpenPalmMinScore
	}
	for label, keyword := range c.Mapping {
		d.Mapping[label] = keyword
	}
	return d
}

func (c GestureKeywordConfig) Validate() error {
	if c.Mode != "legacy" && c.Mode != "keyword" {
		return fmt.Errorf("gesture keyword mode must be legacy or keyword")
	}
	if c.CacheSize < 8 || c.CacheSize > 65536 || c.OpenPalmMaxGapMs < 1 || c.OpenPalmMaxGapMs > 60000 ||
		c.OpenPalmHoldMs < 1 || c.OpenPalmHoldMs > 60000 || math.IsNaN(c.OpenPalmMinScore) ||
		math.IsInf(c.OpenPalmMinScore, 0) || c.OpenPalmMinScore <= 0 || c.OpenPalmMinScore > 1 {
		return fmt.Errorf("invalid gesture keyword cache/age/open-palm limits")
	}
	if c.BaseURL != "" {
		u, err := url.Parse(c.BaseURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" ||
			(u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
			return fmt.Errorf("gesture keyword base_url must be an HTTP(S) origin")
		}
	}
	for gesture, keyword := range c.Mapping {
		if gesture == "" || gesture == "None" || strings.TrimSpace(keyword.TopicLabel) == "" || strings.TrimSpace(keyword.TopicInfo) == "" ||
			len(keyword.TopicLabel) > 128 || len(keyword.TopicInfo) > 128 {
			return fmt.Errorf("invalid gesture keyword mapping for %q", gesture)
		}
	}
	return nil
}

type palmObservation struct {
	first, last int64
	frame       int64
	samples     int
}

// One instance belongs to one session's HTTP sink and is released with that sink.
// Serializing HTTP sends here also serializes retries. Late dispatcher work is
// discarded using source time, so effects cannot regress to an older snapshot.
type gestureKeywordSink struct {
	cfg    GestureKeywordConfig
	mu     sync.Mutex
	scope  string
	latest int64
	seen   map[string]bool
	order  []string
	palms  map[uint64]*palmObservation
}

func (s *HTTPCallbackSink) WithGestureKeywords(cfg GestureKeywordConfig) *HTTPCallbackSink {
	s.keywords = &gestureKeywordSink{cfg: cfg.WithDefaults()}
	return s
}

func keywordResource(r pipeline.RecognitionResult) (*pipeline.MediaResource, error) {
	if r.MediaResources == nil {
		return nil, fmt.Errorf("keyword requires mediaResources")
	}
	m := r.MediaResources
	// Single-leg services retain both resources and send results to MF tAccess.
	// Two-leg services carry only the resource belonging to that session.
	rsrc := m.TAccess
	if rsrc == nil {
		rsrc = m.TCore
	}
	if rsrc == nil || rsrc.ContextID == "" || rsrc.TerminationID == "" ||
		rsrc.ContextID == "." || rsrc.ContextID == ".." || rsrc.TerminationID == "." || rsrc.TerminationID == ".." {
		return nil, fmt.Errorf("keyword requires contextId and terminationId")
	}
	return rsrc, nil
}

func keywordURL(callback, base string, r pipeline.RecognitionResult) (string, error) {
	m, err := keywordResource(r)
	if err != nil {
		return "", err
	}
	path := "/nmf-mrm/v2/contexts/" + url.PathEscape(m.ContextID) + "/termination/" + url.PathEscape(m.TerminationID) + "/keyword"
	u, err := url.Parse(callback)
	if err != nil {
		return "", err
	}
	// A full keyword URL is an explicit override; reject misrouted resources.
	explicit := strings.HasPrefix(u.Path, "/nmf-mrm/") || strings.HasSuffix(u.Path, "/keyword")
	if explicit && (u.EscapedPath() != path || u.RawQuery != "") {
		return "", fmt.Errorf("keyword callback URL does not match media resource")
	}
	if !explicit && base != "" {
		u, err = url.Parse(base)
		if err != nil {
			return "", err
		}
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.Fragment != "" {
		return "", fmt.Errorf("invalid keyword callback URL")
	}
	u.Path, _ = url.PathUnescape(path)
	u.RawPath = path
	if !explicit {
		u.RawQuery = ""
	}
	return u.String(), nil
}

func keywordID(parts ...any) string {
	b, _ := json.Marshal(parts)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

type keywordEvent struct {
	keyword  Keyword
	ids      []string
	eventIDs []string
	gesture  string
	hands    []keywordHand
}

// Only the source fields needed to explain a keyword decision, without geometry.
type keywordHand struct {
	TrackID     uint64  `json:"track_id"`
	RawLabel    string  `json:"raw_label"`
	RawScore    float64 `json:"raw_score"`
	StableLabel string  `json:"stable_label,omitempty"`
	Gesture     string  `json:"gesture"`
}

func (k *gestureKeywordSink) remember(id string) {
	if k.seen[id] {
		return
	}
	if len(k.order) >= k.cfg.CacheSize {
		delete(k.seen, k.order[0])
		k.order = k.order[1:]
	}
	k.seen[id] = true
	k.order = append(k.order, id)
}

func (s *HTTPCallbackSink) sendGestureKeywords(ctx context.Context, r pipeline.RecognitionResult) error {
	k := s.keywords
	k.mu.Lock()
	defer k.mu.Unlock()
	if err := k.cfg.Validate(); err != nil {
		zap.L().Warn("keyword: skipped", zap.String("session_id", r.SessionID),
			zap.String("reason", "invalid_mapping_config"), zap.Error(err))
		return err
	}
	g := r.GestureResult
	if g == nil || !g.Valid() {
		zap.L().Warn("keyword: skipped", zap.String("session_id", r.SessionID),
			zap.String("reason", "invalid_snapshot"))
		return fmt.Errorf("keyword requires a valid gesture snapshot")
	}
	// Small source metadata preserves drain/coverage checks without dumping hands
	// or masks for every frame. Detailed snapshots remain opt-in at debug level.
	zap.L().Info("keyword: before mapping", zap.String("session_id", r.SessionID),
		zap.String("response_id", g.ResponseID), zap.Int64("frame_id", g.FrameID),
		zap.Uint32("rtp_timestamp", g.RTPTimestamp), zap.String("status", g.Status))
	zap.L().Debug("keyword: source details", zap.String("session_id", r.SessionID), zap.Any("gesture_result", g))
	scope := keywordID(r.SessionID, g.MediaEpoch, g.RecognizerEpoch)
	if k.seen == nil {
		k.seen = make(map[string]bool)
	}
	if k.scope != scope {
		k.scope, k.latest = scope, -1
		k.palms = make(map[uint64]*palmObservation)
	}
	if g.TimestampMs < k.latest {
		zap.L().Info("keyword: skipped", zap.String("session_id", r.SessionID),
			zap.String("response_id", g.ResponseID), zap.Int64("frame_id", g.FrameID),
			zap.Int64("timestamp_ms", g.TimestampMs), zap.Int64("latest_timestamp_ms", k.latest),
			zap.String("reason", "late_snapshot"))
		return nil
	}
	k.latest = g.TimestampMs
	if len(g.Hands) == 0 {
		clear(k.palms)
		zap.L().Info("keyword: skipped", zap.String("session_id", r.SessionID), zap.String("response_id", g.ResponseID),
			zap.Int64("frame_id", g.FrameID), zap.String("reason", "no_hands"))
		return nil
	}
	destination, err := keywordURL(s.URL, k.cfg.BaseURL, r)
	if err != nil {
		zap.L().Warn("keyword: skipped", zap.String("session_id", r.SessionID),
			zap.String("response_id", g.ResponseID), zap.Int64("frame_id", g.FrameID),
			zap.String("reason", "invalid_destination"), zap.Error(err))
		return err
	}
	groups := make(map[Keyword]*keywordEvent)
	var events []*keywordEvent
	visible := make(map[uint64]bool)
	hands := append([]pipeline.GestureHand(nil), g.Hands...)
	sort.Slice(hands, func(i, j int) bool { return hands[i].TrackID < hands[j].TrackID })
	for _, h := range hands {
		label, eventID := "", ""
		if a := h.ActiveReaction; a != nil {
			label, eventID = a.Gesture, a.EventID
			delete(k.palms, h.TrackID)
		} else if h.RawLabel == "Open_Palm" && h.RawScore >= k.cfg.OpenPalmMinScore {
			// The worker intentionally excludes Open_Palm from static reactions.
			// Confirm at least three observations over hold_ms before emitting it.
			visible[h.TrackID] = true
			p := k.palms[h.TrackID]
			if p == nil || g.TimestampMs-p.last > k.cfg.OpenPalmMaxGapMs {
				p = &palmObservation{first: g.TimestampMs, last: -1, frame: g.FrameID}
				k.palms[h.TrackID] = p
			}
			if g.TimestampMs > p.last {
				p.samples++
				p.last = g.TimestampMs
			}
			if p.samples >= 3 && g.TimestampMs-p.first >= k.cfg.OpenPalmHoldMs {
				label, eventID = "Open_Palm", fmt.Sprintf("palm/%d/%d", h.TrackID, p.frame)
			}
		}
		if label == "" || label == "None" {
			reason := "no_confirmed_reaction"
			if h.ActiveReaction == nil && h.RawLabel == "Open_Palm" {
				reason = "open_palm_pending"
				if h.RawScore < k.cfg.OpenPalmMinScore {
					reason = "open_palm_low_score"
				}
			}
			zap.L().Info("keyword: skipped", zap.String("session_id", r.SessionID), zap.String("response_id", g.ResponseID),
				zap.Int64("frame_id", g.FrameID), zap.Uint64("track_id", h.TrackID), zap.String("raw_label", h.RawLabel),
				zap.Float64("raw_score", h.RawScore), zap.String("stable_label", h.StableLabel),
				zap.String("gesture", label), zap.String("reason", reason))
			continue
		}
		keyword, ok := k.cfg.Mapping[label]
		if !ok {
			zap.L().Info("keyword: skipped", zap.String("session_id", r.SessionID), zap.String("gesture", label),
				zap.String("response_id", g.ResponseID), zap.Int64("frame_id", g.FrameID), zap.Uint64("track_id", h.TrackID),
				zap.String("raw_label", h.RawLabel), zap.Float64("raw_score", h.RawScore),
				zap.String("stable_label", h.StableLabel), zap.String("event_id", eventID),
				zap.String("reason", "unmapped_gesture"))
			continue
		}
		zap.L().Info("keyword: after mapping", zap.String("session_id", r.SessionID), zap.String("response_id", g.ResponseID),
			zap.Int64("frame_id", g.FrameID), zap.Uint64("track_id", h.TrackID), zap.String("raw_label", h.RawLabel),
			zap.Float64("raw_score", h.RawScore), zap.String("stable_label", h.StableLabel), zap.String("gesture", label),
			zap.String("event_id", eventID), zap.String("topic_label", keyword.TopicLabel), zap.String("topic_info", keyword.TopicInfo))
		id := keywordID(destination, scope, h.TrackID, eventID, keyword)
		group := groups[keyword]
		if group == nil {
			group = &keywordEvent{keyword: keyword, gesture: label}
			groups[keyword] = group
			events = append(events, group)
		}
		group.ids = append(group.ids, id)
		group.eventIDs = append(group.eventIDs, eventID)
		group.hands = append(group.hands, keywordHand{h.TrackID, h.RawLabel, h.RawScore, h.StableLabel, label})
	}
	for track := range k.palms {
		if !visible[track] {
			delete(k.palms, track)
		}
	}
	var failures []error
	for _, event := range events {
		pending := ""
		for _, id := range event.ids {
			if !k.seen[id] {
				pending = id
				break
			}
		}
		if pending == "" {
			zap.L().Info("keyword: skipped", zap.String("session_id", r.SessionID), zap.String("response_id", g.ResponseID),
				zap.Int64("frame_id", g.FrameID), zap.Strings("event_ids", event.eventIDs), zap.Any("keyword", event.keyword),
				zap.String("reason", "duplicate_event"))
			continue
		}
		body, _ := json.Marshal(event.keyword)
		zap.L().Info("keyword: sending", zap.String("session_id", r.SessionID), zap.String("response_id", g.ResponseID),
			zap.Int64("frame_id", g.FrameID), zap.Strings("event_ids", event.eventIDs),
			zap.Any("hands", event.hands),
			zap.String("gesture", event.gesture), zap.String("topic_label", event.keyword.TopicLabel),
			zap.String("topic_info", event.keyword.TopicInfo), zap.String("url", destination), zap.String("idempotency_key", pending))
		if err := s.post(ctx, r, destination, body, pending); err != nil {
			zap.L().Warn("keyword: delivery failed", zap.String("session_id", r.SessionID), zap.String("response_id", g.ResponseID),
				zap.String("idempotency_key", pending), zap.Error(err))
			failures = append(failures, err)
			continue
		}
		zap.L().Info("keyword: delivered", zap.String("session_id", r.SessionID), zap.String("response_id", g.ResponseID),
			zap.Int64("frame_id", g.FrameID), zap.String("idempotency_key", pending), zap.String("url", destination),
			zap.String("topic_label", event.keyword.TopicLabel), zap.String("topic_info", event.keyword.TopicInfo))
		// Only ACKed events become delivered. A later snapshot can retry a failed
		// event with the same key, including after partial multi-hand delivery.
		for _, id := range event.ids {
			k.remember(id)
		}
	}
	return errors.Join(failures...)
}
