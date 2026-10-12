package s3

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/md5"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
)

const (
	partSize = 16 << 20
	maxParts = 10000
)

type Client struct {
	Endpoint  string
	Region    string
	Bucket    string
	AccessKey string
	SecretKey string
	LockDays  int
	HTTP      *http.Client
}

type Object struct {
	Key  string
	Size int64
}

func (c *Client) client() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: 0, Transport: &http.Transport{ResponseHeaderTimeout: time.Minute}}
}

func (c *Client) region() string {
	if c.Region == "" {
		return "us-east-1"
	}
	return c.Region
}

func (c *Client) url(key string, q url.Values) (*url.URL, error) {
	u, err := url.Parse(strings.TrimRight(c.Endpoint, "/"))
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return nil, errors.New("endpoint must be an http or https URL")
	}
	if strings.HasSuffix(u.Hostname(), "amazonaws.com") {
		u.Host = c.Bucket + "." + u.Host
		u.Path = "/" + key
	} else {
		u.Path = "/" + c.Bucket + "/" + key
	}
	u.RawPath = escapePath(u.Path)
	u.RawQuery = strings.ReplaceAll(q.Encode(), "+", "%20")
	return u, nil
}

func escapePath(p string) string {
	segs := strings.Split(p, "/")
	for i, s := range segs {
		segs[i] = strings.ReplaceAll(url.QueryEscape(s), "+", "%20")
	}
	return strings.Join(segs, "/")
}

func hmacSHA(key []byte, s string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(s))
	return h.Sum(nil)
}

func (c *Client) sign(req *http.Request, payloadHash string) {
	now := time.Now().UTC()
	amzDate, day := now.Format("20060102T150405Z"), now.Format("20060102")
	req.Header.Set("x-amz-date", amzDate)
	req.Header.Set("x-amz-content-sha256", payloadHash)
	names := []string{"host"}
	for k := range req.Header {
		if lk := strings.ToLower(k); strings.HasPrefix(lk, "x-amz-") || lk == "content-type" || lk == "content-md5" {
			names = append(names, lk)
		}
	}
	slices.Sort(names)
	var canonHeaders strings.Builder
	for _, n := range names {
		v := req.Header.Get(n)
		if n == "host" {
			v = req.URL.Host
		}
		canonHeaders.WriteString(n + ":" + strings.TrimSpace(v) + "\n")
	}
	signed := strings.Join(names, ";")
	query := strings.ReplaceAll(req.URL.Query().Encode(), "+", "%20")
	canon := strings.Join([]string{req.Method, escapePath(req.URL.Path), query, canonHeaders.String(), signed, payloadHash}, "\n")
	sum := sha256.Sum256([]byte(canon))
	scope := day + "/" + c.region() + "/s3/aws4_request"
	toSign := "AWS4-HMAC-SHA256\n" + amzDate + "\n" + scope + "\n" + hex.EncodeToString(sum[:])
	key := hmacSHA(hmacSHA(hmacSHA(hmacSHA([]byte("AWS4"+c.SecretKey), day), c.region()), "s3"), "aws4_request")
	req.Header.Set("Authorization", fmt.Sprintf("AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s", c.AccessKey, scope, signed, hex.EncodeToString(hmacSHA(key, toSign))))
}

func (c *Client) do(ctx context.Context, method, key string, q url.Values, body []byte, ctype string) (*http.Response, error) {
	u, err := c.url(key, q)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.ContentLength = int64(len(body))
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	if method == http.MethodPut || method == http.MethodPost && body != nil {
		sum := md5.Sum(body)
		req.Header.Set("Content-MD5", base64.StdEncoding.EncodeToString(sum[:]))
	}
	if c.LockDays > 0 && (method == http.MethodPut && q.Get("partNumber") == "" || method == http.MethodPost && q.Has("uploads")) {
		req.Header.Set("x-amz-object-lock-mode", "COMPLIANCE")
		req.Header.Set("x-amz-object-lock-retain-until-date", time.Now().UTC().AddDate(0, 0, c.LockDays).Format(time.RFC3339))
	}
	sum := sha256.Sum256(body)
	c.sign(req, hex.EncodeToString(sum[:]))
	resp, err := c.client().Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		defer resp.Body.Close()
		var e struct {
			Code    string
			Message string
		}
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		if xml.Unmarshal(b, &e) == nil && e.Code != "" {
			return nil, fmt.Errorf("s3 %s %s: %s: %s", method, key, e.Code, e.Message)
		}
		return nil, fmt.Errorf("s3 %s %s: %s", method, key, resp.Status)
	}
	return resp, nil
}

func (c *Client) Put(ctx context.Context, key string, r io.Reader) error {
	buf := make([]byte, partSize)
	n, err := io.ReadFull(r, buf)
	if err == io.EOF || err == io.ErrUnexpectedEOF {
		resp, err := c.do(ctx, http.MethodPut, key, nil, buf[:n], "")
		if err == nil {
			resp.Body.Close()
		}
		return err
	}
	if err != nil {
		return err
	}
	resp, err := c.do(ctx, http.MethodPost, key, url.Values{"uploads": {""}}, nil, "")
	if err != nil {
		return err
	}
	var init struct{ UploadId string }
	err = xml.NewDecoder(resp.Body).Decode(&init)
	resp.Body.Close()
	if err != nil || init.UploadId == "" {
		return fmt.Errorf("s3 multipart upload for %s did not start", key)
	}
	abort := func(err error) error {
		actx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if r, aerr := c.do(actx, http.MethodDelete, key, url.Values{"uploadId": {init.UploadId}}, nil, ""); aerr == nil {
			r.Body.Close()
		}
		return err
	}
	type part struct {
		PartNumber int
		ETag       string
	}
	var parts []part
	for num := 1; n > 0; num++ {
		if num > maxParts {
			return abort(fmt.Errorf("%s is larger than %d GiB", key, partSize*maxParts>>30))
		}
		resp, err := c.do(ctx, http.MethodPut, key, url.Values{"partNumber": {strconv.Itoa(num)}, "uploadId": {init.UploadId}}, buf[:n], "")
		if err != nil {
			return abort(err)
		}
		resp.Body.Close()
		parts = append(parts, part{num, resp.Header.Get("ETag")})
		n, err = io.ReadFull(r, buf)
		if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
			return abort(err)
		}
	}
	var done struct {
		XMLName xml.Name `xml:"CompleteMultipartUpload"`
		Part    []part
	}
	done.Part = parts
	body, _ := xml.Marshal(done)
	resp, err = c.do(ctx, http.MethodPost, key, url.Values{"uploadId": {init.UploadId}}, body, "application/xml")
	if err != nil {
		return abort(err)
	}
	defer resp.Body.Close()
	var result struct{ Code, Message string }
	if b, _ := io.ReadAll(resp.Body); xml.Unmarshal(b, &result) == nil && result.Code != "" {
		return abort(fmt.Errorf("s3 complete %s: %s: %s", key, result.Code, result.Message))
	}
	return nil
}

func (c *Client) Get(ctx context.Context, key string) (*http.Response, error) {
	return c.do(ctx, http.MethodGet, key, nil, nil, "")
}

func (c *Client) Delete(ctx context.Context, key string) error {
	resp, err := c.do(ctx, http.MethodDelete, key, nil, nil, "")
	if err == nil {
		resp.Body.Close()
	}
	return err
}

func (c *Client) List(ctx context.Context, prefix string) ([]Object, error) {
	var out []Object
	token := ""
	for {
		q := url.Values{"list-type": {"2"}, "prefix": {prefix}}
		if token != "" {
			q.Set("continuation-token", token)
		}
		resp, err := c.do(ctx, http.MethodGet, "", q, nil, "")
		if err != nil {
			return nil, err
		}
		var page struct {
			Contents []struct {
				Key  string
				Size int64
			}
			IsTruncated           bool
			NextContinuationToken string
		}
		err = xml.NewDecoder(resp.Body).Decode(&page)
		resp.Body.Close()
		if err != nil {
			return nil, err
		}
		for _, o := range page.Contents {
			out = append(out, Object{o.Key, o.Size})
		}
		if !page.IsTruncated {
			return out, nil
		}
		token = page.NextContinuationToken
	}
}
