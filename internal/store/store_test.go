package store

import (
	"path/filepath"
	"testing"
	"time"
)

func TestStore(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "laavud.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	u, err := s.CreateUser("alice", "admin")
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.UserByHandle(u.Handle)
	if err != nil || got.Name != "alice" || !got.Admin() {
		t.Fatal(got, err)
	}

	tok, _ := s.CreateInvite(Invite{Role: "admin"}, time.Hour)
	if _, err := s.PeekInvite(tok); err != nil {
		t.Fatal(err)
	}
	if err := s.ConsumeInvite(tok); err != nil {
		t.Fatal(err)
	}
	if s.ConsumeInvite(tok) == nil {
		t.Fatal("link must be single use")
	}
	old, _ := s.CreateInvite(Invite{}, -time.Minute)
	if _, err := s.PeekInvite(old); err != ErrNotFound {
		t.Fatal("expired link")
	}

	lt, _ := s.CreateLogin(u.ID, time.Hour, "test")
	if lu, err := s.LoginUser(lt); err != nil || lu.ID != u.ID {
		t.Fatal(err)
	}
	s.DeleteLogin(lt)
	if _, err := s.LoginUser(lt); err != ErrNotFound {
		t.Fatal("sign-out")
	}

	m, _ := s.CreateUser("sasha", "member")
	s.GrantAccess(m.ID, "demo")
	if ok, _ := s.HasAccess(m.ID, "demo"); !ok {
		t.Fatal("access")
	}
	ids, _ := s.UsersWithAccess("demo")
	if len(ids) != 2 {
		t.Fatal("admin and member:", ids)
	}
	ids, _ = s.UsersWithAccess("research")
	if len(ids) != 1 || ids[0] != u.ID {
		t.Fatal("admin only:", ids)
	}

	s.AddCredential(Credential{ID: []byte{1}, UserID: u.ID, Data: []byte("{}")})
	cs, _ := s.Credentials(u.ID)
	if len(cs) != 1 {
		t.Fatal("keys")
	}
	s.DeleteAllAuth()
	cs, _ = s.Credentials(u.ID)
	if len(cs) != 0 {
		t.Fatal("reset-auth")
	}
}
