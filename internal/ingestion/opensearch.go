package ingestion

import (
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

// OpenSearchPoller periodically queries OpenSearch for new ERROR documents and
// feeds them through the shared Ingestor.
type OpenSearchPoller struct {
	client   *opensearchapi.Client
	ingestor *Ingestor
	cfg      *config.Config
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

	return &OpenSearchPoller{
		client:   client,
		ingestor: ingestor,
		cfg:      cfg,
		cron:     cron.New(),
		log:      logger,
	}, nil
}

// Start schedules polling every N seconds. The provided context is used for
// each poll's deadline propagation; call Stop to halt.
func (p *OpenSearchPoller) Start(ctx context.Context) error {
	spec := fmt.Sprintf("@every %ds", p.cfg.OpenSearchPollInterval)
	_, err := p.cron.AddFunc(spec, func() {
		// Each poll gets its own timeout derived from the parent context.
		pollCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		if err := p.poll(pollCtx); err != nil {
			p.log.Error("opensearch poll failed", "err", err)
		}
	})
	if err != nil {
		return fmt.Errorf("opensearch: schedule poll: %w", err)
	}
	p.cron.Start()
	p.log.Info("opensearch poller started", "interval_s", p.cfg.OpenSearchPollInterval, "index", p.cfg.OpenSearchIndex)
	return nil
}

// Stop halts the cron scheduler, waiting for any in-flight poll to finish.
func (p *OpenSearchPoller) Stop() {
	ctx := p.cron.Stop()
	<-ctx.Done()
}

// searchDoc is the subset of source fields we read from each hit. Field names
// follow common ECS-ish conventions with fallbacks handled in mapping.
type searchDoc struct {
	Message    string `json:"message"`
	StackTrace string `json:"stack_trace"`
	Service    string `json:"service"`
	Level      string `json:"level"`
	Severity   string `json:"severity"`
	Timestamp  string `json:"@timestamp"`
}

func (p *OpenSearchPoller) buildQuery() string {
	return fmt.Sprintf(`{
  "size": 50,
  "sort": [{"@timestamp": {"order": "desc"}}],
  "query": {
    "bool": {
      "filter": [
        {"term": {"level": "ERROR"}},
        {"exists": {"field": "stack_trace"}},
        {"range": {"@timestamp": {"gte": "now-%ds"}}}
      ]
    }
  }
}`, p.cfg.OpenSearchLookback)
}

func (p *OpenSearchPoller) poll(ctx context.Context) error {
	resp, err := p.client.Search(ctx, &opensearchapi.SearchReq{
		Indices: []string{p.cfg.OpenSearchIndex},
		Body:    strings.NewReader(p.buildQuery()),
	})
	if err != nil {
		return fmt.Errorf("opensearch: search: %w", err)
	}

	var ingested, skipped int
	for _, hit := range resp.Hits.Hits {
		var doc searchDoc
		if err := json.Unmarshal(hit.Source, &doc); err != nil {
			p.log.Warn("opensearch: skip undecodable hit", "id", hit.ID, "err", err)
			continue
		}
		if strings.TrimSpace(doc.StackTrace) == "" {
			continue
		}

		in := models.IncomingError{
			Message:    doc.Message,
			StackTrace: doc.StackTrace,
			Service:    doc.Service,
			Severity:   firstNonEmpty(doc.Severity, doc.Level),
			Timestamp:  doc.Timestamp,
		}

		_, err := p.ingestor.Ingest(ctx, "opensearch", in)
		if err != nil {
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

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
