package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// FuzzLoad checks config.json decoding on any file content. config.json
// is edited by hand, so it may hold anything: Load must refuse it with
// ErrDecode or return a File whose settings survive being written back
// with Set and loaded again.
func FuzzLoad(f *testing.F) {
	for _, body := range []string{
		`{"capture":"tap","future_key":1,"artwork":true}`,
		`{not json`,
		`{"artwork":"yes"}`,
		`{"future_key":{"a":1}}`,
		`{"library":"~/Archive","capture":"device","artwork":true,"idle_timeout":"5m"}`,
		`{"library":"~/Tapes","capture":"Device","device":" BlackHole 2ch ","idle_timeout":"0","max_failures":4}`,
		`{"capture":"blackhole"}`,
		`{"library":"  "}`,
		`{"device":""}`,
		`{"max_failures":-1}`,
		`{"max_failures":1e400}`,
		`{"library":null}`,
		`{"codec":"aac","aac_bitrate":320,"bit_depth":24,"copy_codec":"any","output_rate":44100}`,
		`{"codec":"ALAC","bit_depth":"16","output_rate":"keep","copy_codec":"Same"}`,
		`{"bit_depth":true}`,
		`{"output_rate":{"hz":44100}}`,
		`{"output_rate":4.41e4}`,
		`{"aac_bitrate":"256"}`,
		`{"bit_depth":null}`,
		`[]`,
		`{}`,
		`null`,
		// A blank file once failed Load although decodeRaw and Set accept it.
		"",
		" \n",
	} {
		f.Add([]byte(body))
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		raw, rawErr := decodeRaw(data)
		if rawErr != nil {
			require.ErrorIs(t, rawErr, ErrDecode)
			assert.Nil(t, raw)
		}

		dir := t.TempDir()
		path := filepath.Join(dir, "config.json")
		require.NoError(t, os.WriteFile(path, data, 0o600))
		file, warnings, err := Load(path)
		if err != nil {
			require.ErrorIs(t, err, ErrDecode)
			if rawErr == nil {
				// decodeRaw found well-formed JSON, so only a value of the
				// wrong type may fail the second decode.
				var typeErr *json.UnmarshalTypeError
				require.ErrorAs(t, err, &typeErr, "decodeRaw accepted %q but Load failed", data)
			}
			return
		}
		require.NoError(t, rawErr, "Load accepted what decodeRaw refuses")
		unknown := 0
		for k := range raw {
			if _, known := lookup(k); !known {
				unknown++
			}
		}
		assert.Len(t, warnings, unknown, "one warning per unknown key")

		want, err := Resolve(file, nil, Overrides{}, testHome)
		if err != nil {
			require.ErrorIs(t, err, ErrInvalidValue)
			return
		}
		again := filepath.Join(dir, "again.json")
		for _, st := range table() {
			if v, ok := fieldText(st.inFile(&file)); ok {
				require.NoError(t, Set(again, st.Name, v, testHome), "Set refuses %s=%q that Resolve accepted", st.Name, v)
			}
		}
		reloaded, warnings, err := Load(again)
		require.NoError(t, err)
		assert.Empty(t, warnings)
		got, err := Resolve(reloaded, nil, Overrides{}, testHome)
		require.NoError(t, err)
		assert.Equal(t, want, got)
	})
}
