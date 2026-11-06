package ingestion

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/opensearch-project/opensearch-go/v4"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"
	"github.com/robfig/cron/v3"

	"github.com/yourorg/logai/internal/config"
	"github.com/yourorg/logai/internal/models"
)

// searchPageSize is the maximum number of hits fetched per poll.
const searchPageSize = 50

// FieldMap names the document fields the poller reads. Dotted paths such as
// "error.stack_trace" match both nested objects and flat dotted keys.
type FieldMap struct {
	Level      string
	LevelValue string
	Message    string
	StackTrace string
	Service    string
	Timestamp  string
}

// FieldMapFromConfig builds a FieldMap from the configured field names.
func FieldMapFromConfig(cfg *config.Config) FieldMap {
	return FieldMap{
		Level:      cfg.OpenSearchLevelField,
		LevelValue: cfg.OpenSearchLevelValue,
		Message:    cfg.OpenSearchMessageField,
		StackTrace: cfg.OpenSearchStackTraceField,
		Service:    cfg.OpenSearchServiceField,
		Timestamp:  cfg.OpenSearchTimestampField,
	}
}

// OpenSearchPoller periodically queries OpenSearch for new ERROR documents and
// feeds them through the shared Ingestor.
type OpenSearchPoller struct {
	client   *opensearchapi.Client
	ingestor *Ingestor
	cfg      *config.Config
	fields   FieldMap
	cron     *cron.Cron
	log      *slog.Logger
}

// NewOpenSearchPoller builds the poller and its underlying OpenSearch client.
func NewOpenSearchPoller(cfg *config.Config, ingestor *Ingestor, logger *slog.Logger) (*OpenSearchPoller, error) {
	if logger == nil {
		logger = slog.Default()
	}
	client, err := opensearchapi.NewClient(opensearchapi.Config{
		Client: opensearch.Config{
			Addresses: []string{cfg.OpenSearchURL},
			Username:  cfg.OpenSearchUsername,
			Password:  cfg.OpenSearchPassword,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("opensearch: new client: %w", err)
	}

	cronLogger := cron.PrintfLogger(slog.NewLogLogger(logger.Handler(), slog.LevelDebug))
	return &OpenSearchPoller{
		client:   client,
		ingestor: ingestor,
		cfg:      cfg,
		fields:   FieldMapFromConfig(cfg),
		// SkipIfStillRunning: a slow poll must not stack up overlapping polls.
		cron: cron.New(cron.WithChain(cron.SkipIfStillRunning(cronLogger))),
		log:  logger,
	}, nil
}

// Start runs one poll right away, then every N seconds. The provided context
// is used for each poll's deadline propagation; call Stop to halt.
func (p *OpenSearchPoller) Start(ctx context.Context) error {
	pollOnce := func() {
		// Each poll gets its own timeout derived from the parent context.
		pollCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		if err := p.poll(pollCtx); err != nil {
			p.log.Error("opensearch poll failed", "err", err)
		}
	}

	spec := fmt.Sprintf("@every %ds", p.cfg.OpenSearchPollInterval)
	entryID, err := p.cron.AddFunc(spec, pollOnce)
	if err != nil {
		return fmt.Errorf("opensearch: schedule poll: %w", err)
	}
	p.cron.Start()
	// Kick off the first poll immediately (through the cron chain, so it
	// still can't overlap with a scheduled run).
	go p.cron.Entry(entryID).WrappedJob.Run()

	p.log.Info("opensearch poller started",
		"interval_s", p.cfg.OpenSearchPollInterval, "index", p.cfg.OpenSearchIndex,
		"level_field", p.fields.Level, "stack_trace_field", p.fields.StackTrace)
	return nil
}

// Stop halts the cron scheduler, waiting for any in-flight poll to finish.
func (p *OpenSearchPoller) Stop() {
	ctx := p.cron.Stop()
	<-ctx.Done()
}

// BuildQuery returns the search body for one poll.
func BuildQuery(f FieldMap, lookbackSeconds int) ([]byte, error) {
	q := map[string]any{
		"size": searchPageSize,
		"sort": []any{map[string]any{f.Timestamp: map[string]string{"order": "desc"}}},
		"query": map[string]any{
			"bool": map[string]any{
				"filter": []any{
					map[string]any{"term": map[string]string{f.Level: f.LevelValue}},
					map[string]any{"exists": map[string]string{"field": f.StackTrace}},
					map[string]any{"range": map[string]any{
						f.Timestamp: map[string]string{"gte": fmt.Sprintf("now-%ds", lookbackSeconds)},
					}},
				},
			},
		},
	}
	return json.Marshal(q)
}

// MapDocument converts one hit's _source into an IncomingError using the
// field map. ok is false when the document has no usable stack trace.
func MapDocument(f FieldMap, source []byte) (in models.IncomingError, ok bool, err error) {
	var doc map[string]any
	if err := json.Unmarshal(source, &doc); err != nil {
		return models.IncomingError{}, false, err
	}
	in = models.IncomingError{
		Message:    lookupString(doc, f.Message),
		StackTrace: lookupString(doc, f.StackTrace),
		Service:    lookupString(doc, f.Service),
		Severity:   lookupString(doc, f.Level),
		Timestamp:  lookupString(doc, f.Timestamp),
	}
	if strings.TrimSpace(in.StackTrace) == "" {
		return in, false, nil
	}
	if strings.TrimSpace(in.Message) == "" {
		// Fall back to the first stack line so the error has a usable title.
		in.Message = strings.TrimSpace(strings.SplitN(strings.TrimSpace(in.StackTrace), "\n", 2)[0])
	}
	return in, true, nil
}

func (p *OpenSearchPoller) poll(ctx context.Context) error {
	body, err := BuildQuery(p.fields, p.cfg.OpenSearchLookback)
	if err != nil {
		return fmt.Errorf("opensearch: build query: %w", err)
	}
	resp, err := p.client.Search(ctx, &opensearchapi.SearchReq{
		Indices: []string{p.cfg.OpenSearchIndex},
		Body:    bytes.NewReader(body),
	})
	if err != nil {
		return fmt.Errorf("opensearch: search: %w", err)
	}

	var ingested, skipped int
	for _, hit := range resp.Hits.Hits {
		in, ok, err := MapDocument(p.fields, hit.Source)
		if err != nil {
			p.log.Warn("opensearch: skip undecodable hit", "id", hit.ID, "err", err)
			continue
		}
		if !ok {
			continue
		}

		if _, err := p.ingestor.Ingest(ctx, "opensearch", in); err != nil {
			if errors.Is(err, ErrDuplicate) {
				skipped++
				continue
			}
			p.log.Error("opensearch: ingest hit failed", "id", hit.ID, "err", err)
			continue
		}
		ingested++
	}

	if ingested > 0 || skipped > 0 {
		p.log.Info("opensearch poll complete", "hits", len(resp.Hits.Hits), "ingested", ingested, "skipped_dup", skipped)
	}
	return nil
}

// lookupString resolves a dotted path in a decoded JSON document. A flat key
// containing dots ("log.level") wins over nested traversal ({"log":{"level"}}).
func lookupString(doc map[string]any, path string) string {
	if path == "" {
		return ""
	}
	if v, ok := doc[path]; ok {
		return stringify(v)
	}
	head, rest, found := strings.Cut(path, ".")
	if !found {
		return ""
	}
	// Try progressively longer flat prefixes: "a.b.c" -> doc["a"]["b.c"], doc["a.b"]["c"].
	for {
		if child, ok := doc[head].(map[string]any); ok {
			if s := lookupString(child, rest); s != "" {
				return s
			}
		}
		next, remainder, more := strings.Cut(rest, ".")
		if !more {
			return ""
		}
		head, rest = head+"."+next, remainder
	}
}

func stringify(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case []any:
		parts := make([]string, 0, len(t))
		for _, e := range t {
			parts = append(parts, stringify(e))
		}
		return strings.Join(parts, "\n")
	default:
		return fmt.Sprint(t)
	}
}
