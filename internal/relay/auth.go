package relay

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"slices"
	"strings"
	"sync"
	"time"
)

// How a member gets in. The administrator makes an invitation for a
// member; its code is shown once and kept only as a hash. The member
// redeems it with a password of their own, which signs them in; from then
// on they sign in by name and password. Signed in, a member makes device
// keys, each shown once, and takes any of them away without touching the
// others.

const (
	// InvitePrefix and SessionPrefix start an invitation's code and a
	// session's token, so neither is taken for the other or for a key.
	InvitePrefix  = "inv-"
	SessionPrefix = "ses-"
	// InviteLife is how long an invitation can be redeemed.
	InviteLife = 72 * time.Hour
	// SessionLife is how long a sign-in lasts.
	SessionLife = 7 * 24 * time.Hour
	// minPassword is the shortest password taken.
	minPassword = 10
	maxSessions = 8
)

// PassRounds is PBKDF2's rounds for a new password. A test lowers it; a
// password is checked with the rounds it was made with.
var PassRounds = 600_000

// errSignIn is all a failed sign-in is told: the name, the password and a
// member turned off are not told apart.
var errSignIn = errors.New("The name or the password is not right")

// ErrThrottled is a sign-in or a redemption refused for too many wrong
// tries.
var ErrThrottled = errors.New("Too many wrong tries: wait 15 minutes and try again")

func random(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func hashOf(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func passwordHash(pass, salt string, iter int) (string, error) {
	raw, err := hex.DecodeString(salt)
	if err != nil {
		return "", err
	}
	k, err := pbkdf2.Key(sha256.New, pass, raw, iter, 32)
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(k), nil
}

func (m *Member) setPassword(pass string) error {
	if len(pass) < minPassword || len(pass) > 256 {
		return errors.New("Use a password of at least 10 characters")
	}
	salt, err := random(16)
	if err != nil {
		return err
	}
	h, err := passwordHash(pass, salt, PassRounds)
	if err != nil {
		return err
	}
	m.PassHash, m.PassSalt, m.PassIter = h, salt, PassRounds
	return nil
}

func (m *Member) checkPassword(pass string) bool {
	if m.PassHash == "" || m.PassSalt == "" || m.PassIter <= 0 {
		return false
	}
	h, err := passwordHash(pass, m.PassSalt, m.PassIter)
	return err == nil && subtle.ConstantTimeCompare([]byte(h), []byte(m.PassHash)) == 1
}

// attempts are the wrong tries made of one thing: a member's name, or the
// address invitations are tried from.
type attempts struct {
	n     int
	first time.Time
}

var throttle = struct {
	sync.Mutex
	m map[string]*attempts
}{m: map[string]*attempts{}}

const (
	throttleTries  = 5
	throttleWindow = 15 * time.Minute
)

// throttled says what has had too many wrong tries of late.
func throttled(what string, now time.Time) bool {
	throttle.Lock()
	defer throttle.Unlock()
	a := throttle.m[what]
	if a == nil {
		return false
	}
	if now.Sub(a.first) > throttleWindow {
		delete(throttle.m, what)
		return false
	}
	return a.n >= throttleTries
}

func failed(what string, now time.Time) {
	throttle.Lock()
	defer throttle.Unlock()
	a := throttle.m[what]
	if a == nil || now.Sub(a.first) > throttleWindow {
		a = &attempts{first: now}
		throttle.m[what] = a
	}
	a.n++
	if len(throttle.m) > 4096 { // someone trying names: the oldest go
		for k, v := range throttle.m {
			if now.Sub(v.first) > throttleWindow {
				delete(throttle.m, k)
			}
		}
	}
}

func passed(what string) {
	throttle.Lock()
	delete(throttle.m, what)
	throttle.Unlock()
}

// Invite makes an invitation for a member and returns its code, shown this
// once. An invitation made before it for the member no longer works.
func Invite(member string) (code string, expires time.Time, err error) {
	token, err := random(20)
	if err != nil {
		return "", time.Time{}, err
	}
	code = InvitePrefix + token
	expires = time.Now().Add(InviteLife)
	err = change(func(st *State) error {
		if st.member(member) == nil {
			return errors.New("Member not found")
		}
		id, err := random(6)
		if err != nil {
			return err
		}
		now := time.Now().Unix()
		st.Invites = slices.DeleteFunc(st.Invites, func(i invite) bool { return i.Member == member || i.Expires <= now })
		st.Invites = append(st.Invites, invite{ID: id, Member: member, Hash: hashOf(code), Expires: expires.Unix()})
		return nil
	})
	return code, expires, err
}

// Redeem uses an invitation: the member takes pass as their password and
// is signed in. The code works once. from is where the try came from, for
// the limit on wrong ones.
func Redeem(code, pass, from string) (token string, m Member, err error) {
	code = strings.TrimSpace(code)
	now := time.Now()
	what := "invite\x00" + from
	if throttled(what, now) {
		return "", Member{}, ErrThrottled
	}
	sum := hashOf(code)
	err = change(func(st *State) error {
		i := slices.IndexFunc(st.Invites, func(i invite) bool {
			return subtle.ConstantTimeCompare([]byte(i.Hash), []byte(sum)) == 1
		})
		if i < 0 || st.Invites[i].Expires <= now.Unix() {
			return errors.New("This invitation is not valid: it was used, replaced or has run out. Ask for a new one")
		}
		mem := st.member(st.Invites[i].Member)
		if mem == nil || mem.Off {
			return errors.New("This invitation is not valid: it was used, replaced or has run out. Ask for a new one")
		}
		if err := mem.setPassword(pass); err != nil {
			return err // the invitation is kept for a better password
		}
		st.Invites = slices.Delete(st.Invites, i, i+1)
		// a password set anew ends the sign-ins made with the old one
		st.Sessions = slices.DeleteFunc(st.Sessions, func(s session) bool { return s.Member == mem.ID })
		token, err = st.signIn(mem.ID, now)
		m = mem.public()
		return err
	})
	if err != nil {
		if !strings.HasPrefix(err.Error(), "Use a password") {
			failed(what, now)
		}
		return "", Member{}, err
	}
	passed(what)
	return token, m, nil
}

// signIn adds a session for the member and returns its token.
func (st *State) signIn(member string, now time.Time) (string, error) {
	t, err := random(24)
	if err != nil {
		return "", err
	}
	token := SessionPrefix + t
	st.Sessions = slices.DeleteFunc(st.Sessions, func(s session) bool { return s.Expires <= now.Unix() })
	// the oldest of a member's sign-ins make room for a new one
	mine := 0
	for _, s := range st.Sessions {
		if s.Member == member {
			mine++
		}
	}
	for i := 0; mine >= maxSessions && i < len(st.Sessions); {
		if st.Sessions[i].Member == member {
			st.Sessions = slices.Delete(st.Sessions, i, i+1)
			mine--
			continue
		}
		i++
	}
	st.Sessions = append(st.Sessions, session{Hash: hashOf(token), Member: member, Expires: now.Add(SessionLife).Unix()})
	return token, nil
}

// spare is a password check made for a name no member has, so that one
// takes as long to refuse as a wrong password does.
var spare = sync.OnceValue(func() Member {
	var m Member
	_ = m.setPassword("no member has this password")
	return m
})

// Login signs a member in by name and password.
func Login(name, pass string) (token string, m Member, err error) {
	name = strings.ToLower(strings.TrimSpace(name))
	now := time.Now()
	what := "login\x00" + name
	if throttled(what, now) {
		return "", Member{}, ErrThrottled
	}
	st, err := read()
	if err != nil {
		return "", Member{}, err
	}
	mem := st.memberNamed(name)
	if mem == nil || mem.Name != name || mem.Off || mem.PassHash == "" {
		sp := spare()
		sp.checkPassword(pass)
		failed(what, now)
		return "", Member{}, errSignIn
	}
	if !mem.checkPassword(pass) {
		failed(what, now)
		return "", Member{}, errSignIn
	}
	id := mem.ID
	err = change(func(st *State) error {
		cur := st.member(id)
		if cur == nil || cur.Off {
			return errSignIn
		}
		m = cur.public()
		token, err = st.signIn(id, now)
		return err
	})
	if err != nil {
		return "", Member{}, err
	}
	passed(what)
	return token, m, nil
}

// SessionMember is the member a session's token signs in, while the
// session lasts and the member is on.
func SessionMember(token string) (Member, bool) {
	if !strings.HasPrefix(token, SessionPrefix) {
		return Member{}, false
	}
	st, err := read()
	if err != nil {
		return Member{}, false
	}
	sum, now := hashOf(token), time.Now().Unix()
	for _, s := range st.Sessions {
		if s.Expires > now && subtle.ConstantTimeCompare([]byte(s.Hash), []byte(sum)) == 1 {
			m := st.member(s.Member)
			if m == nil || m.Off {
				return Member{}, false
			}
			return m.public(), true
		}
	}
	return Member{}, false
}

// Logout ends a session.
func Logout(token string) error {
	sum := hashOf(token)
	return change(func(st *State) error {
		st.Sessions = slices.DeleteFunc(st.Sessions, func(s session) bool { return s.Hash == sum })
		return nil
	})
}

// ChangePassword gives a member a new password for the old one, and ends
// the member's other sign-ins: keep is the session that stays.
func ChangePassword(member, old, pass, keep string) error {
	what := "login\x00" + member
	now := time.Now()
	if throttled(what, now) {
		return ErrThrottled
	}
	sum := hashOf(keep)
	err := change(func(st *State) error {
		m := st.member(member)
		if m == nil || !m.checkPassword(old) {
			return errSignIn
		}
		if err := m.setPassword(pass); err != nil {
			return err
		}
		st.Sessions = slices.DeleteFunc(st.Sessions, func(s session) bool { return s.Member == member && s.Hash != sum })
		return nil
	})
	if errors.Is(err, errSignIn) {
		failed(what, now)
	}
	return err
}
