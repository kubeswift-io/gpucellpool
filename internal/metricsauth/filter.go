// Package metricsauth authenticates and authorizes the metrics endpoint using
// only client-go, which this operator already depends on.
//
// # Why not controller-runtime's filter
//
// The obvious answer is `filters.WithAuthenticationAndAuthorization` from
// controller-runtime. It was measured rather than assumed: importing it takes
// this operator from 829 to 1078 compiled packages — +249, a 30% increase —
// dragging in k8s.io/apiserver, gRPC internals, cel-go and OpenTelemetry
// exporters. That is the cost of an apiserver-grade authenticator with
// webhook caching, anonymous-auth policy and OIDC, none of which a metrics port
// needs.
//
// What it protects is modest by the issue's own account: pool and namespace
// names, cell phases, capacity figures. No credentials, no guest data. Paying a
// 30% dependency increase — in an operator whose first design principle is
// avoiding exactly that — to guard that is the wrong trade.
//
// So this does the same two API calls the upstream filter does, directly:
// TokenReview to establish who is calling, SubjectAccessReview to ask whether
// they may read this path. Same security property, zero new packages.
//
// # What it deliberately does NOT do
//
// No client-certificate authentication (upstream supports it). Prometheus
// scrapes with a bearer token, and a second auth path is a second thing to get
// wrong. A cert-authenticated scraper gets 401 and a log line saying why.
//
// No anonymous access, ever — that is the hole being closed.
package metricsauth

// The two calls the filter makes. They are declared here, next to the code that
// makes them, rather than on the reconciler that does not.
//
// +kubebuilder:rbac:groups=authentication.k8s.io,resources=tokenreviews,verbs=create
// +kubebuilder:rbac:groups=authorization.k8s.io,resources=subjectaccessreviews,verbs=create

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"
	authenticationv1 "k8s.io/api/authentication/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

// cacheTTL bounds how long an allow decision is reused.
//
// Prometheus scrapes on a fixed interval (30s by default), and without a cache
// every scrape costs two apiserver round trips forever. Two minutes keeps that
// to roughly one pair per scraper per two minutes.
//
// Only ALLOW is cached, and only for the exact (token, path) pair. A denial is
// never cached: the usual reason a denial becomes an allow is that an operator
// just fixed the RBAC, and making them wait out a TTL to see it work is how a
// security control gets switched off in frustration.
const cacheTTL = 2 * time.Minute

// verb is what a scrape is, in SubjectAccessReview terms. A metrics read is an
// HTTP GET, which maps to "get" on a non-resource URL.
const verb = "get"

type entry struct {
	expires time.Time
	user    string
}

// Authorizer answers "may this request read metrics" via the apiserver.
type Authorizer struct {
	client kubernetes.Interface
	log    logr.Logger

	mu    sync.Mutex
	allow map[string]entry
	now   func() time.Time // injectable for tests
}

// NewFilterProvider returns a controller-runtime metrics FilterProvider that
// requires a bearer token whose owner is authorized for the requested path.
func NewFilterProvider() func(*rest.Config, *http.Client) (metricsserver.Filter, error) {
	return func(c *rest.Config, httpClient *http.Client) (metricsserver.Filter, error) {
		cs, err := kubernetes.NewForConfigAndClient(c, httpClient)
		if err != nil {
			return nil, err
		}
		return New(cs).Filter, nil
	}
}

// New builds an Authorizer over any client-go interface, so tests can pass a fake.
func New(cs kubernetes.Interface) *Authorizer {
	return &Authorizer{client: cs, allow: map[string]entry{}, now: time.Now}
}

// Filter is the metricsserver.Filter: it wraps the metrics handler.
func (a *Authorizer) Filter(log logr.Logger, handler http.Handler) (http.Handler, error) {
	a.log = log
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		user, err := a.check(req)
		if err != nil {
			// The status distinguishes the two failures on purpose: 401 means
			// "you did not prove who you are", 403 means "you did, and you may
			// not". An operator debugging a scrape needs to know which.
			code := http.StatusForbidden
			if errors.Is(err, errUnauthenticated) {
				code = http.StatusUnauthorized
			}
			// Logged at V(1), not Error: a probe hitting the port is routine
			// noise, not an operator-actionable fault, and logging it loudly is
			// how a metrics endpoint becomes a log-flood amplifier.
			log.V(1).Info("metrics request rejected", "code", code, "reason", err.Error(),
				"path", req.URL.Path, "remote", req.RemoteAddr)
			w.WriteHeader(code)
			return
		}
		log.V(2).Info("metrics request allowed", "user", user, "path", req.URL.Path)
		handler.ServeHTTP(w, req)
	}), nil
}

var (
	errUnauthenticated = errors.New("no valid bearer token")
	errUnauthorized    = errors.New("not authorized for this path")
)

func (a *Authorizer) check(req *http.Request) (string, error) {
	token := bearerToken(req)
	if token == "" {
		return "", errUnauthenticated
	}
	path := req.URL.Path
	if user, ok := a.cached(token, path); ok {
		return user, nil
	}

	ctx, cancel := context.WithTimeout(req.Context(), 10*time.Second)
	defer cancel()

	tr, err := a.client.AuthenticationV1().TokenReviews().Create(ctx,
		&authenticationv1.TokenReview{Spec: authenticationv1.TokenReviewSpec{Token: token}},
		metav1.CreateOptions{})
	if err != nil {
		// A failed review is NOT an allow. If the apiserver cannot tell us who
		// this is, the answer is no.
		return "", errUnauthenticated
	}
	if !tr.Status.Authenticated {
		return "", errUnauthenticated
	}
	u := tr.Status.User

	sar, err := a.client.AuthorizationV1().SubjectAccessReviews().Create(ctx,
		&authorizationv1.SubjectAccessReview{Spec: authorizationv1.SubjectAccessReviewSpec{
			User:   u.Username,
			UID:    u.UID,
			Groups: u.Groups,
			NonResourceAttributes: &authorizationv1.NonResourceAttributes{
				Path: path,
				Verb: verb,
			},
		}},
		metav1.CreateOptions{})
	if err != nil || !sar.Status.Allowed {
		return "", errUnauthorized
	}

	a.remember(token, path, u.Username)
	return u.Username, nil
}

// bearerToken extracts the token, requiring the scheme match case-insensitively
// as RFC 7235 specifies.
func bearerToken(req *http.Request) string {
	h := req.Header.Get("Authorization")
	const prefix = "bearer "
	if len(h) <= len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
		return ""
	}
	return strings.TrimSpace(h[len(prefix):])
}

// cacheKey binds a decision to BOTH the token and the path. Keying on the token
// alone would let a caller authorized for one path read another.
func cacheKey(token, path string) string { return token + "\x00" + path }

func (a *Authorizer) cached(token, path string) (string, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	e, ok := a.allow[cacheKey(token, path)]
	if !ok || !a.now().Before(e.expires) {
		return "", false
	}
	return e.user, true
}

func (a *Authorizer) remember(token, path, user string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	// Bound the map. A caller cycling tokens must not grow it without limit;
	// dropping everything is fine because the entries are only an optimisation.
	if len(a.allow) > 1024 {
		a.allow = map[string]entry{}
	}
	a.allow[cacheKey(token, path)] = entry{expires: a.now().Add(cacheTTL), user: user}
}
