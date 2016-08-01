package api

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"

	"github.com/gorilla/mux"
	"github.com/readium/readium-lcp-server/crypto"
	"github.com/readium/readium-lcp-server/epub"
	"github.com/readium/readium-lcp-server/license"
	"github.com/readium/readium-lcp-server/sign"

	"io"
	"net/http"
)

//{
//"content_key": "12345",
//"date": "2013-11-04T01:08:15+01:00",
//"hint": "Enter your email address",
//"hint_url": "http://www.imaginaryebookretailer.com/lcp"
//}

func GenerateLicense(w http.ResponseWriter, r *http.Request, s Server) {
	vars := mux.Vars(r)
	var lic license.License

	err := decodeJsonLicense(r, &lic)

	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	key := vars["key"]

	w.Header().Add("Content-Type", "application/vnd.readium.lcp.license.1-0+json")
	w.Header().Add("Content-Disposition", `attachment; filename="license.lcpl"`)

	err = completeLicense(&lic, key, s)

	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	err = s.Licenses().Add(lic)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	lic.Encryption.UserKey.Check = nil

	enc := json.NewEncoder(w)
	enc.Encode(lic)
}

func GenerateProtectedPublication(w http.ResponseWriter, r *http.Request, s Server) {
	vars := mux.Vars(r)

	var lic license.License

	err := decodeJsonLicense(r, &lic)

	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	key := vars["key"]

	item, err := s.Store().Get(key)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	content, err := s.Index().Get(key)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	var b bytes.Buffer
	contents, err := item.Contents()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	io.Copy(&b, contents)
	zr, err := zip.NewReader(bytes.NewReader(b.Bytes()), int64(b.Len()))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	ep, err := epub.Read(zr)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	var buf bytes.Buffer

	err = completeLicense(&lic, key, s)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	lic.Links["publication"] = license.Link{Href: item.PublicUrl(), Type: "application/epub+zip"}
	lic.ContentId = key

	enc := json.NewEncoder(&buf)
	enc.Encode(lic)

	err = s.Licenses().Add(lic)

	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	ep.Add("META-INF/license.lcpl", &buf, uint64(buf.Len()))
	w.Header().Add("Content-Type", "application/epub+zip")
	w.Header().Add("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, content.Location))
	ep.Write(w)

}

func decodeJsonLicense(r *http.Request, lic *license.License) error {
	var dec *json.Decoder

	if ctype := r.Header["Content-Type"]; len(ctype) > 0 && ctype[0] == "application/x-www-form-urlencoded" {
		buf := bytes.NewBufferString(r.PostFormValue("data"))
		dec = json.NewDecoder(buf)
	} else {
		dec = json.NewDecoder(r.Body)
	}

	err := dec.Decode(&lic)

	return err
}

func completeLicense(l *license.License, key string, s Server) error {
	c, err := s.Index().Get(key)
	if err != nil {
		return err
	}

	license.Prepare(l)
	l.ContentId = key

	var encryptionKey []byte
	if len(l.Encryption.UserKey.Value) > 0 {
		encryptionKey = l.Encryption.UserKey.Value
		l.Encryption.UserKey.Value = nil
	} else {
		passphrase := l.Encryption.UserKey.ClearValue
		l.Encryption.UserKey.ClearValue = ""
		hash := sha256.Sum256([]byte(passphrase))
		encryptionKey = hash[:]
	}

	l.Encryption.ContentKey.Algorithm = "http://www.w3.org/2001/04/xmlenc#aes256-cbc"
	l.Encryption.ContentKey.Value = encryptKey(c.EncryptionKey, encryptionKey[:])

	l.Encryption.UserKey.Algorithm = "http://www.w3.org/2001/04/xmlenc#sha256"

	err = encryptFields(l, encryptionKey[:])
	if err != nil {
		return err
	}
	err = buildKeyCheck(l, encryptionKey[:])
	if err != nil {
		return err
	}
	err = signLicense(l, s.Certificate())
	if err != nil {
		return err
	}
	return nil
}

func buildKeyCheck(l *license.License, key []byte) error {
	var out bytes.Buffer
	err := crypto.Encrypt(key, bytes.NewBufferString(l.Id), &out)
	if err != nil {
		return err
	}
	l.Encryption.UserKey.Check = out.Bytes()
	return nil
}

func encryptFields(l *license.License, key []byte) error {
	for _, toEncrypt := range l.User.Encrypted {
		var out bytes.Buffer
		field := getField(&l.User, toEncrypt)
		err := crypto.Encrypt(key[:], bytes.NewBufferString(field.String()), &out)
		if err != nil {
			return err
		}
		field.Set(reflect.ValueOf(base64.StdEncoding.EncodeToString(out.Bytes())))
	}
	return nil
}

func getField(u *license.UserInfo, field string) reflect.Value {
	v := reflect.ValueOf(u).Elem()
	return v.FieldByName(strings.Title(field))
}

func signLicense(l *license.License, cert *tls.Certificate) error {
	sig, err := sign.NewSigner(cert)
	if err != nil {
		return err
	}
	res, err := sig.Sign(l)
	if err != nil {
		return err
	}
	l.Signature = &res

	return nil
}

func encryptKey(key []byte, kek []byte) []byte {
	var out bytes.Buffer
	in := bytes.NewReader(key)
	crypto.Encrypt(kek[:], in, &out)
	return out.Bytes()
}
