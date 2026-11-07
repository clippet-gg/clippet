package httpapi

import "net/http"

// Refusing an upload without reading it (ROADMAP #32, §20 erratum 138).
//
// Every upload route answers 401/403/429/413/415/507 from MIDDLEWARE or from
// beginUpload — before a single byte of the body has been read. That has
// always been true (nothing in the credential chain touches r.Body), and it is
// the property that matters: an anonymous client with a revoked ShareX token
// or no token at all cannot make this server spool a gigabyte before telling
// it no.
//
// What was missing is what happens to the CONNECTION afterwards. The client is
// still sending; net/http will try to drain what is left so the connection can
// be reused, and only gives up past its own internal threshold. On a body that
// declares 1 GiB — or declares nothing at all, which is worse, because there
// is no number to compare — that drain is exactly the transfer the refusal
// exists to avoid. So the refusal says `Connection: close`, which makes
// net/http skip the drain, write the answer and close the socket (after its
// own half-close delay, which is what still lets the client read the 401 it
// asked for rather than a reset).
//
// The header is PRE-SET rather than added on the way out. A ResponseWriter
// wrapper would have to be threaded through http.MaxBytesReader, whose
// too-large handling recognises only net/http's own writer, and a request that
// is accepted would then have to un-say something already written. Pre-setting
// inverts it: the middleware marks the answer closable, and the ONE place that
// is about to read the body — beginUpload, once every gate has passed — takes
// the mark off again. Every exit above that point keeps it.

// unreadBodyCloseBytes is the declared length past which a refusal closes the
// connection. It matches net/http's own maxPostHandlerReadBytes: below it the
// server drains the remainder in one read and the connection stays useful to a
// capture tool that is about to retry with a good credential, which is the
// case this threshold protects.
const unreadBodyCloseBytes = 256 << 10

// closeUnlessRead pre-sets `Connection: close` for a request whose body is too
// big to drain — or whose length is not declared at all (`Transfer-Encoding:
// chunked`, which net/http reports as −1, the case with no upper bound). It is
// the OUTERMOST middleware on every upload route, so it covers the credential
// refusals, the CSRF refusal and both rate limiters as well as the handler.
func closeUnlessRead(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ContentLength < 0 || r.ContentLength > unreadBodyCloseBytes {
			w.Header().Set("Connection", "close")
		}
		next.ServeHTTP(w, r)
	})
}

// bodyWillBeRead cancels that mark. It is called exactly once, at the end of
// beginUpload, when the request has passed every gate and the multipart reader
// is about to be handed to a caller that streams the body to scratch: a
// connection whose body IS consumed is a connection worth keeping.
func bodyWillBeRead(w http.ResponseWriter) { w.Header().Del("Connection") }
