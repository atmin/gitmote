package webui

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Session state is a signed cookie, not a server-side table: the value is
// "<userID>.<expiryUnix>.<hmac>", where the HMAC-SHA256 over "<userID>.<expiry>"
// is keyed by GITMOTE_COOKIE_KEY. This keeps the UI stateless — no sessions
// table, no new source of truth — at the cost of not being able to revoke a
// single cookie before it expires; the short TTL bounds that, and the admin
// middleware re-checks the user (and is_admin) on every request.
const (
	cookieName = "gm_session"
	sessionTTL = 12 * time.Hour
)

type sessions struct {
	key []byte
}

// issue signs a session for userID and sets it as the response cookie. Secure is
// set only on TLS requests so plain-HTTP local development still works while a
// real (HTTPS) deployment gets a Secure cookie.
func (s *sessions) issue(w http.ResponseWriter, r *http.Request, userID int64, now time.Time) {
	exp := now.Add(sessionTTL).Unix()
	payload := strconv.FormatInt(userID, 10) + "." + strconv.FormatInt(exp, 10)
	value := payload + "." + s.sign(payload)
	http.SetCookie(w, &http.Cookie{
		Name:     cookieName,
		Value:    value,
		Path:     "/",
		Expires:  now.Add(sessionTTL),
		HttpOnly: true,
		Secure:   r.TLS != nil,
		SameSite: http.SameSiteLaxMode,
	})
}

// verify returns the userID carried by a valid, unexpired session cookie.
func (s *sessions) verify(r *http.Request, now time.Time) (int64, bool) {
	c, err := r.Cookie(cookieName)
	if err != nil {
		return 0, false
	}
	uid, expStr, mac, ok := splitCookie(c.Value)
	if !ok {
		return 0, false
	}
	payload := uid + "." + expStr
	if subtle.ConstantTimeCompare([]byte(mac), []byte(s.sign(payload))) != 1 {
		return 0, false
	}
	exp, err := strconv.ParseInt(expStr, 10, 64)
	if err != nil || now.Unix() >= exp {
		return 0, false
	}
	id, err := strconv.ParseInt(uid, 10, 64)
	if err != nil {
		return 0, false
	}
	return id, true
}

// clear expires the session cookie on logout.
func (s *sessions) clear(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     cookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
}

func (s *sessions) sign(payload string) string {
	mac := hmac.New(sha256.New, s.key)
	mac.Write([]byte(payload))
	return hex.EncodeToString(mac.Sum(nil))
}

func splitCookie(v string) (uid, exp, mac string, ok bool) {
	parts := strings.Split(v, ".")
	if len(parts) != 3 {
		return "", "", "", false
	}
	return parts[0], parts[1], parts[2], true
}
