package metricsauth

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	authenticationv1 "k8s.io/api/authentication/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// clientFor builds a fake clientset whose TokenReview/SubjectAccessReview answer
// as told, and counts the calls so caching can be observed.
func clientFor(authned, allowed bool) (*fake.Clientset, *int32, *int32) {
	var trCalls, sarCalls int32
	cs := fake.NewSimpleClientset()
	cs.PrependReactor("create", "tokenreviews", func(k8stesting.Action) (bool, runtime.Object, error) {
		atomic.AddInt32(&trCalls, 1)
		return true, &authenticationv1.TokenReview{Status: authenticationv1.TokenReviewStatus{
			Authenticated: authned,
			User:          authenticationv1.UserInfo{Username: "system:serviceaccount:monitoring:prometheus"},
		}}, nil
	})
	cs.PrependReactor("create", "subjectaccessreviews", func(k8stesting.Action) (bool, runtime.Object, error) {
		atomic.AddInt32(&sarCalls, 1)
		return true, &authorizationv1.SubjectAccessReview{
			Status: authorizationv1.SubjectAccessReviewStatus{Allowed: allowed},
		}, nil
	})
	return cs, &trCalls, &sarCalls
}

func serve(t *testing.T, a *Authorizer, req *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	served := false
	h, err := a.Filter(log.Log, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		served = true
		_, _ = w.Write([]byte("# HELP metrics\n"))
	}))
	if err != nil {
		t.Fatalf("Filter: %v", err)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code == http.StatusOK != served {
		t.Fatalf("handler reached=%v but status=%d", served, rec.Code)
	}
	return rec
}

func withToken(tok string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	if tok != "" {
		r.Header.Set("Authorization", "Bearer "+tok)
	}
	return r
}

// The hole being closed: before this, no token meant the series were served.
func TestNoTokenIsRejected(t *testing.T) {
	cs, tr, _ := clientFor(true, true)
	if rec := serve(t, New(cs), withToken("")); rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 for an anonymous request", rec.Code)
	}
	if *tr != 0 {
		t.Error("an anonymous request should not cost an apiserver round trip")
	}
}

// 401 and 403 must be distinguishable — an operator debugging a scrape needs to
// know whether the token was bad or the RBAC was.
func TestUnauthenticatedIs401AndUnauthorizedIs403(t *testing.T) {
	cs, _, _ := clientFor(false, true)
	if rec := serve(t, New(cs), withToken("bad")); rec.Code != http.StatusUnauthorized {
		t.Errorf("unauthenticated: status = %d, want 401", rec.Code)
	}
	cs2, _, _ := clientFor(true, false)
	if rec := serve(t, New(cs2), withToken("good")); rec.Code != http.StatusForbidden {
		t.Errorf("authenticated but not allowed: status = %d, want 403", rec.Code)
	}
}

func TestAuthorizedRequestIsServed(t *testing.T) {
	cs, _, _ := clientFor(true, true)
	rec := serve(t, New(cs), withToken("good"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if rec.Body.String() == "" {
		t.Error("authorized request got no body")
	}
}

// An apiserver that cannot answer must not be treated as an allow. This is the
// fail-open direction, and it is the one that matters.
func TestApiserverErrorDeniesRatherThanAllows(t *testing.T) {
	cs := fake.NewSimpleClientset()
	cs.PrependReactor("create", "tokenreviews", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, http.ErrServerClosed
	})
	if rec := serve(t, New(cs), withToken("good")); rec.Code == http.StatusOK {
		t.Fatal("a failed TokenReview served the metrics; it must deny")
	}
}

// Caching exists so a 30s scrape does not cost two API calls forever.
func TestAllowIsCachedButDenialIsNot(t *testing.T) {
	cs, tr, sar := clientFor(true, true)
	a := New(cs)
	for i := 0; i < 3; i++ {
		if rec := serve(t, a, withToken("good")); rec.Code != http.StatusOK {
			t.Fatalf("call %d: status %d", i, rec.Code)
		}
	}
	if *tr != 1 || *sar != 1 {
		t.Errorf("3 allowed scrapes cost tr=%d sar=%d calls, want 1 and 1", *tr, *sar)
	}

	// A denial must be re-checked every time: the usual reason it becomes an
	// allow is that an operator just fixed the RBAC, and making them wait out a
	// TTL is how a security control gets switched off in frustration.
	csD, trD, _ := clientFor(true, false)
	d := New(csD)
	for i := 0; i < 3; i++ {
		serve(t, d, withToken("good"))
	}
	if *trD != 3 {
		t.Errorf("denials were cached (tr=%d, want 3); a fixed RBAC would not take effect", *trD)
	}
}

// The cache key must bind the path, not just the token — otherwise a token
// authorized for one path reads another.
func TestCacheDoesNotLeakAcrossPaths(t *testing.T) {
	cs, _, sar := clientFor(true, true)
	a := New(cs)
	serve(t, a, withToken("good")) // /metrics

	other := httptest.NewRequest(http.MethodGet, "/debug/pprof/heap", nil)
	other.Header.Set("Authorization", "Bearer good")
	serve(t, a, other)

	if *sar != 2 {
		t.Fatalf("SubjectAccessReview ran %d times for two different paths, want 2 — "+
			"a cache keyed on the token alone would authorize a path nobody checked", *sar)
	}
}

func TestCacheExpires(t *testing.T) {
	cs, tr, _ := clientFor(true, true)
	a := New(cs)
	clock := time.Now()
	a.now = func() time.Time { return clock }

	serve(t, a, withToken("good"))
	clock = clock.Add(cacheTTL + time.Second)
	serve(t, a, withToken("good"))

	if *tr != 2 {
		t.Errorf("tr calls = %d, want 2: an expired entry must be re-checked", *tr)
	}
}

// RFC 7235 says the scheme is case-insensitive; a scraper sending "bearer" must
// not be rejected for it.
func TestBearerSchemeIsCaseInsensitive(t *testing.T) {
	for _, scheme := range []string{"Bearer", "bearer", "BEARER"} {
		cs, _, _ := clientFor(true, true)
		r := httptest.NewRequest(http.MethodGet, "/metrics", nil)
		r.Header.Set("Authorization", scheme+" good")
		if rec := serve(t, New(cs), r); rec.Code != http.StatusOK {
			t.Errorf("scheme %q rejected with %d", scheme, rec.Code)
		}
	}
	// But a different scheme is not a bearer token.
	cs, _, _ := clientFor(true, true)
	r := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	r.Header.Set("Authorization", "Basic Zm9vOmJhcg==")
	if rec := serve(t, New(cs), r); rec.Code != http.StatusUnauthorized {
		t.Errorf("Basic auth accepted as a bearer token: %d", rec.Code)
	}
}
