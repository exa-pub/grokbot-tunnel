package oauth

import "testing"

// A login is accepted only with both tokens.
func FuzzParseLogin(f *testing.F) {
	f.Add([]byte(`{"accessToken":"a","refreshToken":"r","authId":"u","selectedTeamId":42}`))
	f.Add([]byte(`{"accessToken":"a","refreshToken":""}`))
	f.Add([]byte(`{"selectedTeamId":"x"}`))
	f.Add([]byte(`[]`))
	f.Fuzz(func(t *testing.T, body []byte) {
		id, err := parseLogin(body)
		if err == nil && (id.AccessToken == "" || id.RefreshToken == "") {
			t.Fatalf("%+v from %q", id, body)
		}
	})
}
