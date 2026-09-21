package observability

import (
	"context"
	"net/http"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/Ferousco-dev/mailx/internal/buildinfo"
)

// Finite label vocabularies. Every label value passes through pick(), so an
// unexpected value becomes "other" and cardinality can never grow with input.
var (
	methods        = []string{"GET", "POST", "PUT", "PATCH", "DELETE"}
	statusClasses  = []string{"1xx", "2xx", "3xx", "4xx", "5xx"}
	sessionResults = []string{"completed", "failed", "rejected"}
	messageResults = []string{"accepted", "rejected", "temporary_failure"}
	kinds          = []string{"accepted", "invalid_request", "dns_not_found", "dns_null_mx", "dns_temporary",
		"dns_failure", "transfer_temporary", "transfer_permanent", "context"}
	decisions   = []string{"retry", "terminal_success", "terminal_failure"}
	queueOps    = []string{"enqueue", "claim", "ack", "release", "renew", "persist"}
	okErr       = []string{"ok", "error"}
	whOutcomes  = []string{"succeeded", "retrying", "failed"}
	tlsPolicies = []string{"opportunistic", "required"}
	tlsOutcomes = []string{"established", "not_offered", "required_unavailable", "rejected", "handshake_timeout",
		"verify_failed", "handshake_failed", "connection_lost", "ehlo_failed", "canceled", "protocol_error"}
	tlsVersions  = []string{"1.2", "1.3", "other", "none"}
	dkimAlgs     = []string{"rsa-sha256"}
	dkimOutcomes = []string{"signed", "unsigned_no_key", "key_unavailable", "key_decrypt_failed", "key_invalid",
		"sign_failed", "domain_mismatch"}
	authMechs    = []string{"plain", "login", "none"}
	authOutcomes = []string{"success", "rejected", "temporary", "no_tls", "not_advertised", "no_mechanism",
		"protocol_error", "connection_lost", "timeout", "canceled"}
)

func pick(v string, allowed []string) string {
	for _, a := range allowed {
		if v == a {
			return v
		}
	}
	return "other"
}

// DepthFunc reports available+claimed queue jobs without mutating the queue.
type DepthFunc func(context.Context) (int64, error)

// Metrics owns a private registry. A nil *Metrics is valid and inert, and
// every method swallows panics so metrics can never affect the caller.
type Metrics struct {
	reg          *prometheus.Registry
	httpTotal    *prometheus.CounterVec
	httpDuration *prometheus.HistogramVec
	smtpSessions *prometheus.CounterVec
	smtpActive   prometheus.Gauge
	smtpMessages *prometheus.CounterVec
	deliveries   *prometheus.CounterVec
	deliveryDur  *prometheus.HistogramVec
	queueOps     *prometheus.CounterVec
	depthErrs    atomic.Uint64
	depthErrDesc *prometheus.Desc
	webhooks     *prometheus.CounterVec
	webhookDur   *prometheus.HistogramVec
	tlsSessions  *prometheus.CounterVec
	authAttempts *prometheus.CounterVec
	dkimSigs     *prometheus.CounterVec
	depthFn      DepthFunc
	depthDesc    *prometheus.Desc
}

func NewMetrics(info buildinfo.Info) (*Metrics, error) {
	ns := "mailx"
	m := &Metrics{reg: prometheus.NewPedanticRegistry()}
	m.httpTotal = prometheus.NewCounterVec(prometheus.CounterOpts{Namespace: ns, Name: "http_requests_total",
		Help: "Developer API requests."}, []string{"method", "route", "status_class"})
	m.httpDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{Namespace: ns, Name: "http_request_duration_seconds",
		Help: "Developer API request duration.", Buckets: prometheus.DefBuckets}, []string{"method", "route"})
	m.smtpSessions = prometheus.NewCounterVec(prometheus.CounterOpts{Namespace: ns, Name: "smtp_sessions_total",
		Help: "SMTP sessions by result."}, []string{"result"})
	m.smtpActive = prometheus.NewGauge(prometheus.GaugeOpts{Namespace: ns, Name: "smtp_active_sessions",
		Help: "Currently open SMTP sessions."})
	m.smtpMessages = prometheus.NewCounterVec(prometheus.CounterOpts{Namespace: ns, Name: "smtp_messages_total",
		Help: "Inbound SMTP messages by result."}, []string{"result"})
	m.deliveries = prometheus.NewCounterVec(prometheus.CounterOpts{Namespace: ns, Name: "delivery_attempts_total",
		Help: "Delivery operations by outcome kind and lifecycle decision."}, []string{"kind", "decision"})
	m.deliveryDur = prometheus.NewHistogramVec(prometheus.HistogramOpts{Namespace: ns, Name: "delivery_attempt_duration_seconds",
		Help: "Delivery operation duration.", Buckets: []float64{.1, .5, 1, 2.5, 5, 10, 30, 60, 120}}, []string{"decision"})
	m.queueOps = prometheus.NewCounterVec(prometheus.CounterOpts{Namespace: ns, Name: "queue_operations_total",
		Help: "Queue operations by result."}, []string{"operation", "result"})
	m.depthErrDesc = prometheus.NewDesc(ns+"_queue_depth_errors_total", "Failed queue depth collections.", nil, nil)
	m.webhooks = prometheus.NewCounterVec(prometheus.CounterOpts{Namespace: ns, Name: "webhook_attempts_total",
		Help: "Webhook delivery attempts by outcome."}, []string{"outcome"})
	m.webhookDur = prometheus.NewHistogramVec(prometheus.HistogramOpts{Namespace: ns, Name: "webhook_attempt_duration_seconds",
		Help: "Webhook attempt duration.", Buckets: []float64{.05, .1, .25, .5, 1, 2.5, 5, 10}}, []string{"outcome"})
	m.tlsSessions = prometheus.NewCounterVec(prometheus.CounterOpts{Namespace: ns, Name: "smtp_tls_sessions_total",
		Help: "Outbound SMTP TLS decisions by policy, bounded outcome and negotiated version."},
		[]string{"policy", "outcome", "version"})
	m.authAttempts = prometheus.NewCounterVec(prometheus.CounterOpts{Namespace: ns, Name: "smtp_auth_attempts_total",
		Help: "Outbound SMTP AUTH results by bounded mechanism and outcome."}, []string{"mechanism", "outcome"})
	m.dkimSigs = prometheus.NewCounterVec(prometheus.CounterOpts{Namespace: ns, Name: "dkim_signatures_total",
		Help: "DKIM signing attempts by bounded algorithm and outcome."}, []string{"algorithm", "outcome"})
	m.depthDesc = prometheus.NewDesc(ns+"_queue_depth", "Queue jobs available plus claimed.", nil, nil)
	build := prometheus.NewGaugeVec(prometheus.GaugeOpts{Namespace: ns, Name: "build_info",
		Help: "Build identity; value is always 1."}, []string{"version", "commit"})
	build.WithLabelValues(info.Version, info.Commit).Set(1)

	for _, c := range []prometheus.Collector{m.httpTotal, m.httpDuration, m.smtpSessions, m.smtpActive, m.smtpMessages,
		m.deliveries, m.deliveryDur, m.queueOps, m.tlsSessions, m.authAttempts, m.dkimSigs, m.webhooks, m.webhookDur, build, m,
		collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{})} {
		if err := m.reg.Register(c); err != nil {
			return nil, err
		}
	}
	return m, nil
}

// Handler serves the Prometheus text exposition of this registry only.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.reg, promhttp.HandlerOpts{})
}

// Registry returns the private registry (tests use it to gather families).
func (m *Metrics) Registry() *prometheus.Registry { return m.reg }

// SetQueueDepth installs the read-only depth source; call before serving.
func (m *Metrics) SetQueueDepth(fn DepthFunc) {
	if m != nil {
		m.depthFn = fn
	}
}

// Describe/Collect make Metrics the queue-depth collector. A failed or slow
// depth read bumps a bounded error counter and emits no sample.
func (m *Metrics) Describe(ch chan<- *prometheus.Desc) {
	ch <- m.depthDesc
	ch <- m.depthErrDesc
}

// Both series come from this one collector so the error count is always
// consistent with the sample (or absence of one) emitted in the same scrape.
func (m *Metrics) Collect(ch chan<- prometheus.Metric) {
	if m.depthFn != nil {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		depth, err := m.safeDepth(ctx)
		cancel()
		if err != nil {
			m.depthErrs.Add(1)
		} else {
			ch <- prometheus.MustNewConstMetric(m.depthDesc, prometheus.GaugeValue, float64(depth))
		}
	}
	ch <- prometheus.MustNewConstMetric(m.depthErrDesc, prometheus.CounterValue, float64(m.depthErrs.Load()))
}

func (m *Metrics) safeDepth(ctx context.Context) (n int64, err error) {
	defer func() {
		if recover() != nil {
			err = context.Canceled
		}
	}()
	return m.depthFn(ctx)
}

func guard() { _ = recover() }

func (m *Metrics) HTTPRequest(method, route string, status int, d time.Duration) {
	if m == nil {
		return
	}
	defer guard()
	class := "other"
	if status >= 100 && status < 600 {
		class = strconv.Itoa(status/100) + "xx"
	}
	method = pick(method, methods)
	m.httpTotal.WithLabelValues(method, route, pick(class, statusClasses)).Inc()
	m.httpDuration.WithLabelValues(method, route).Observe(d.Seconds())
}

func (m *Metrics) SMTPSessionStarted() {
	if m != nil {
		defer guard()
		m.smtpActive.Inc()
	}
}

func (m *Metrics) SMTPSessionEnded(result string) {
	if m != nil {
		defer guard()
		m.smtpActive.Dec()
		m.smtpSessions.WithLabelValues(pick(result, sessionResults)).Inc()
	}
}

func (m *Metrics) SMTPSessionRejected() {
	if m != nil {
		defer guard()
		m.smtpSessions.WithLabelValues("rejected").Inc()
	}
}

func (m *Metrics) SMTPMessage(result string) {
	if m != nil {
		defer guard()
		m.smtpMessages.WithLabelValues(pick(result, messageResults)).Inc()
	}
}

func (m *Metrics) DeliveryAttempt(kind, decision string, d time.Duration) {
	if m != nil {
		defer guard()
		m.deliveries.WithLabelValues(pick(kind, kinds), pick(decision, decisions)).Inc()
		m.deliveryDur.WithLabelValues(pick(decision, decisions)).Observe(d.Seconds())
	}
}

func (m *Metrics) QueueOp(op string, err error) {
	if m != nil {
		defer guard()
		result := "ok"
		if err != nil {
			result = "error"
		}
		m.queueOps.WithLabelValues(pick(op, queueOps), pick(result, okErr)).Inc()
	}
}

func (m *Metrics) WebhookAttempt(outcome string, d time.Duration) {
	if m != nil {
		defer guard()
		m.webhooks.WithLabelValues(pick(outcome, whOutcomes)).Inc()
		m.webhookDur.WithLabelValues(pick(outcome, whOutcomes)).Observe(d.Seconds())
	}
}

// TLSResult records one outbound SMTP TLS decision. It satisfies
// smtp.TLSObserver. Every value is allowlisted; no host, address or error text
// can become a label.
func (m *Metrics) TLSResult(policy, outcome, version string) {
	if m != nil {
		defer guard()
		m.tlsSessions.WithLabelValues(pick(policy, tlsPolicies), pick(outcome, tlsOutcomes), pick(version, tlsVersions)).Inc()
	}
}

// AuthResult records one outbound SMTP AUTH result. It satisfies
// smtp.AuthObserver. Labels are allowlisted: no user name, host or error text
// can ever become a label.
func (m *Metrics) AuthResult(mechanism, outcome string) {
	if m != nil {
		defer guard()
		m.authAttempts.WithLabelValues(pick(mechanism, authMechs), pick(outcome, authOutcomes)).Inc()
	}
}

// SignResult records one DKIM signing attempt. It satisfies dkim.Observer.
// Labels are allowlisted: no domain, selector, key data or error text can ever
// become a label.
func (m *Metrics) SignResult(algorithm, outcome string) {
	if m != nil {
		defer guard()
		m.dkimSigs.WithLabelValues(pick(algorithm, dkimAlgs), pick(outcome, dkimOutcomes)).Inc()
	}
}
