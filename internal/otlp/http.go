package otlp

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	colmetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	statuspb "google.golang.org/genproto/googleapis/rpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/lutherleo/burnrate/internal/engine"
)

// Path is the OTLP/HTTP metrics path.
const Path = "/v1/metrics"

// Content types defined by the OTLP/HTTP specification.
const (
	ContentTypeProtobuf = "application/x-protobuf"
	ContentTypeJSON     = "application/json"
)

// DefaultMaxBodyBytes caps a decompressed request body.
const DefaultMaxBodyBytes = 16 << 20

// ReceiverStats are cumulative counters exposed on /metrics.
type ReceiverStats struct {
	Requests       atomic.Uint64
	RequestErrors  atomic.Uint64
	PointsAccepted atomic.Uint64
	PointsRejected atomic.Uint64
	PointsIgnored  atomic.Uint64
}

// Receiver is an http.Handler for POST /v1/metrics.
type Receiver struct {
	tr      *Translator
	sink    func([]engine.Sample)
	maxBody int64
	log     *slog.Logger
	Stats   ReceiverStats
}

// NewReceiver returns a Receiver that hands translated samples to sink.
func NewReceiver(tr *Translator, sink func([]engine.Sample), log *slog.Logger) *Receiver {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Receiver{tr: tr, sink: sink, maxBody: DefaultMaxBodyBytes, log: log}
}

// SetMaxBodyBytes changes the decompressed body limit.
func (rc *Receiver) SetMaxBodyBytes(n int64) { rc.maxBody = n }

func (rc *Receiver) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rc.Stats.Requests.Add(1)
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		rc.fail(w, ContentTypeJSON, http.StatusMethodNotAllowed, "only POST is supported")
		return
	}
	ct, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || (ct != ContentTypeProtobuf && ct != ContentTypeJSON) {
		rc.fail(w, ContentTypeJSON, http.StatusUnsupportedMediaType,
			fmt.Sprintf("unsupported content type %q; use %s or %s", r.Header.Get("Content-Type"), ContentTypeProtobuf, ContentTypeJSON))
		return
	}
	body, status, err := rc.readBody(r)
	if err != nil {
		rc.fail(w, ct, status, err.Error())
		return
	}
	req := &colmetricspb.ExportMetricsServiceRequest{}
	if ct == ContentTypeProtobuf {
		err = proto.Unmarshal(body, req)
	} else {
		err = protojson.UnmarshalOptions{DiscardUnknown: true}.Unmarshal(body, req)
	}
	if err != nil {
		rc.fail(w, ct, http.StatusBadRequest, "cannot decode ExportMetricsServiceRequest: "+err.Error())
		return
	}
	samples, res := rc.tr.Translate(req)
	if len(samples) > 0 {
		rc.sink(samples)
	}
	rc.Stats.PointsAccepted.Add(uint64(res.Points - res.Rejected))
	rc.Stats.PointsRejected.Add(uint64(res.Rejected))
	rc.Stats.PointsIgnored.Add(uint64(res.Ignored))
	resp := &colmetricspb.ExportMetricsServiceResponse{}
	if res.Rejected > 0 {
		resp.PartialSuccess = &colmetricspb.ExportMetricsPartialSuccess{
			RejectedDataPoints: int64(res.Rejected),
			ErrorMessage:       strings.Join(res.Errors, "; "),
		}
		rc.log.Warn("rejected OTLP data points", "count", res.Rejected, "errors", res.Errors)
	}
	rc.write(w, ct, http.StatusOK, resp)
}

func (rc *Receiver) readBody(r *http.Request) ([]byte, int, error) {
	var reader io.Reader = http.MaxBytesReader(nil, r.Body, rc.maxBody)
	switch enc := strings.ToLower(r.Header.Get("Content-Encoding")); enc {
	case "", "identity":
	case "gzip":
		gz, err := gzip.NewReader(reader)
		if err != nil {
			return nil, http.StatusBadRequest, fmt.Errorf("invalid gzip body: %w", err)
		}
		defer func() { _ = gz.Close() }()
		reader = gz
	default:
		return nil, http.StatusUnsupportedMediaType, fmt.Errorf("unsupported content encoding %q", enc)
	}
	// Bound the decompressed size too, so a small gzip bomb cannot expand
	// past the limit.
	body, err := io.ReadAll(io.LimitReader(reader, rc.maxBody+1))
	var tooBig *http.MaxBytesError
	switch {
	case errors.As(err, &tooBig) || int64(len(body)) > rc.maxBody:
		return nil, http.StatusRequestEntityTooLarge, fmt.Errorf("request body exceeds %d bytes", rc.maxBody)
	case err != nil:
		return nil, http.StatusBadRequest, fmt.Errorf("reading body: %w", err)
	}
	return body, 0, nil
}

func (rc *Receiver) fail(w http.ResponseWriter, ct string, code int, msg string) {
	rc.Stats.RequestErrors.Add(1)
	rc.log.Debug("OTLP request failed", "status", code, "error", msg)
	rc.write(w, ct, code, &statuspb.Status{Code: grpcCode(code), Message: msg})
}

func (rc *Receiver) write(w http.ResponseWriter, ct string, code int, m proto.Message) {
	var (
		b   []byte
		err error
	)
	if ct == ContentTypeProtobuf {
		b, err = proto.Marshal(m)
	} else {
		b, err = protojson.Marshal(m)
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", ct)
	w.WriteHeader(code)
	if _, err := w.Write(b); err != nil {
		rc.log.Debug("writing OTLP response", "error", err)
	}
}

// grpcCode maps an HTTP status to the google.rpc.Code carried in Status.
func grpcCode(httpStatus int) int32 {
	switch httpStatus {
	case http.StatusBadRequest:
		return 3 // INVALID_ARGUMENT
	case http.StatusRequestEntityTooLarge:
		return 8 // RESOURCE_EXHAUSTED
	case http.StatusMethodNotAllowed, http.StatusUnsupportedMediaType:
		return 12 // UNIMPLEMENTED
	}
	return 2 // UNKNOWN
}

// Client exports metrics over OTLP/HTTP with protobuf encoding.
type Client struct {
	Endpoint string // base URL, e.g. http://127.0.0.1:4318
	HTTP     *http.Client
	Gzip     bool
	// MaxRetries bounds retries of retryable failures (429, 502, 503, 504).
	MaxRetries int
	// Backoff is the initial retry delay, doubled per attempt. Default 100ms.
	Backoff time.Duration
}

// Export sends one request and returns the server's response.
func (c *Client) Export(ctx context.Context, req *colmetricspb.ExportMetricsServiceRequest) (*colmetricspb.ExportMetricsServiceResponse, error) {
	payload, err := proto.Marshal(req)
	if err != nil {
		return nil, err
	}
	if c.Gzip {
		var buf bytes.Buffer
		zw := gzip.NewWriter(&buf)
		if _, err := zw.Write(payload); err != nil {
			return nil, err
		}
		if err := zw.Close(); err != nil {
			return nil, err
		}
		payload = buf.Bytes()
	}
	hc := c.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	backoff := c.Backoff
	if backoff == 0 {
		backoff = 100 * time.Millisecond
	}
	for attempt := 0; ; attempt++ {
		resp, retry, err := c.do(ctx, hc, payload)
		if err == nil || !retry || attempt >= c.MaxRetries {
			return resp, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(backoff << attempt):
		}
	}
}

func (c *Client) do(ctx context.Context, hc *http.Client, payload []byte) (*colmetricspb.ExportMetricsServiceResponse, bool, error) {
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(c.Endpoint, "/")+Path, bytes.NewReader(payload))
	if err != nil {
		return nil, false, err
	}
	hreq.Header.Set("Content-Type", ContentTypeProtobuf)
	if c.Gzip {
		hreq.Header.Set("Content-Encoding", "gzip")
	}
	resp, err := hc.Do(hreq)
	if err != nil {
		return nil, true, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, DefaultMaxBodyBytes))
	if err != nil {
		return nil, true, err
	}
	if resp.StatusCode != http.StatusOK {
		st := &statuspb.Status{}
		msg := strconv.Quote(string(body))
		if proto.Unmarshal(body, st) == nil && st.GetMessage() != "" {
			msg = st.GetMessage()
		}
		retry := false
		switch resp.StatusCode {
		case http.StatusTooManyRequests, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
			retry = true
		}
		return nil, retry, fmt.Errorf("otlp export: HTTP %d: %s", resp.StatusCode, msg)
	}
	out := &colmetricspb.ExportMetricsServiceResponse{}
	if err := proto.Unmarshal(body, out); err != nil {
		return nil, false, fmt.Errorf("otlp export: decoding response: %w", err)
	}
	return out, false, nil
}
