// main.go - corrected, robust-ish no-JS speed test
package main

import (
	"io"
	"log"
	"math"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"
)

type Sess struct {
	mu         sync.Mutex
	LastSent   time.Time
	Pings      []float64
	DownloadB  float64
	UploadB    float64
	ClientHost string
}

var (
	sm       sync.Mutex
	sessions = map[string]*Sess{}
)

func mkid() string { return strconv.FormatInt(time.Now().UnixNano(), 36) }

func idxGet(sid string) *Sess {
	sm.Lock()
	defer sm.Unlock()
	if s, ok := sessions[sid]; ok {
		return s
	}
	s := &Sess{}
	sessions[sid] = s
	return s
}

func getIP(r *http.Request) string {
	if ip := r.Header.Get("X-Forwarded-For"); ip != "" {
		return ip
	}
	h, _, _ := net.SplitHostPort(r.RemoteAddr)
	return h
}

func root(w http.ResponseWriter, r *http.Request) {
	ip := getIP(r)
	io.WriteString(w, `<!doctype html><html><head><meta charset="utf-8"><title>tiny-speedtest</title></head><body>
	<h2>tiny-speedtest (no JS)</h2>
	<form method="POST" action="/start"><input type="hidden" name="ip" value="`+ip+`"><button>Start test</button></form>
	<p>Download test default: 8 MiB. If automatic download doesn't start, click the link on the next page.</p>
	</body></html>`)
}

// start: create session and respond with a page that meta-refreshes to the first probe
func start(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", 400)
		return
	}
	sid := mkid()
	s := idxGet(sid)
	s.mu.Lock()
	s.ClientHost = r.FormValue("ip")
	s.LastSent = time.Now()
	s.mu.Unlock()
	// quick page that sends client to first probe via meta-refresh (no redirect loops)
	html := `<!doctype html><html><head><meta charset="utf-8"><meta http-equiv="refresh" content="0;url=/probe?sid=` + sid + `&n=1"></head><body>Starting…</body></html>`
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write([]byte(html))
}

// probe: count probes via a simple meta-refresh chain; final page starts download via iframe + link
func probe(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	sid := q.Get("sid")
	n, _ := strconv.Atoi(q.Get("n"))
	if sid == "" || n < 1 {
		http.Error(w, "bad", 400)
		return
	}
	s := idxGet(sid)
	now := time.Now()
	s.mu.Lock()
	if !s.LastSent.IsZero() {
		delta := now.Sub(s.LastSent).Seconds() * 1000
		s.Pings = append(s.Pings, delta)
	}
	s.LastSent = now
	s.mu.Unlock()

	if n < 8 {
		next := n + 1
		// short meta-refresh to next probe step
		html := `<!doctype html><html><head><meta charset="utf-8"><meta http-equiv="refresh" content="0;url=/probe?sid=` + sid + `&n=` + strconv.Itoa(next) + `"></head><body>ping ` + strconv.Itoa(n) + `</body></html>`
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(html))
		return
	}

	// final probe: produce a page that includes a nonce'd download URL, an iframe to trigger fetch,
	// and a visible link (fallback) — avoids caching and gives manual fallback for picky browsers.
	nonce := mkid()
	size := 8 * 1024 * 1024
	dl := "/download?sid=" + sid + "&size=" + strconv.Itoa(size) + "&nonce=" + nonce
	html := `<!doctype html><html><head><meta charset="utf-8"><title>download</title></head><body>
	<h3>Starting download test</h3>
	<p>If download does not start automatically, click the link below.</p>
	<p><a href="` + dl + `">Click here to download test file</a></p>
	<!-- hidden iframe: most browsers will fetch the src; some tiny browsers may not -->
	<iframe src="` + dl + `" style="display:none"></iframe>
	<p>Results page will appear after the server records the download (or after a short timeout).</p>
	<meta http-equiv="refresh" content="1;url=/results?sid=` + sid + `">
	</body></html>`
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write([]byte(html))
}

// download: stream bytes, prevent caching, record server-side bps and log it
func download(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	sid := q.Get("sid")
	size, _ := strconv.Atoi(q.Get("size"))
	if size <= 0 {
		size = 8 * 1024 * 1024
	}

	// prevent caches/proxies from serving cached payload
	w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate, max-age=0")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Expires", "0")
	// set content-type octet-stream; we provide an iframe + link fallback so browsers will fetch the resource
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.Itoa(size))

	start := time.Now()
	chunk := make([]byte, 64*1024)
	for i := range chunk {
		chunk[i] = 'a'
	}
	bw := 0
	flusher, _ := w.(http.Flusher)
	for bw < size {
		to := size - bw
		if to > len(chunk) {
			to = len(chunk)
		}
		n, err := w.Write(chunk[:to])
		if err != nil {
			// client closed; break
			break
		}
		bw += n
		if flusher != nil {
			flusher.Flush()
		}
	}
	elapsed := time.Since(start).Seconds()
	if elapsed < 1e-9 {
		elapsed = 1e-9
	}
	bps := float64(bw) / elapsed

	if sid != "" {
		s := idxGet(sid)
		s.mu.Lock()
		s.DownloadB = bps
		s.mu.Unlock()
	}
	log.Printf("download done sid=%s bytes=%d elapsed=%.3fs bps=%.3fMiB/s\n", sid, bw, elapsed, bps/1024/1024)
}

// upload: same as before, measure time to receive uploaded file(s)
func upload(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(32 << 20); err != nil && err != http.ErrNotMultipart {
		// continue even if not multipart
	}
	sid := r.FormValue("sid")
	if sid == "" {
		sid = r.URL.Query().Get("sid")
	}
	s := idxGet(sid)
	start := time.Now()
	var n int64
	if r.MultipartForm != nil {
		for _, fhs := range r.MultipartForm.File {
			for _, fh := range fhs {
				f, err := fh.Open()
				if err == nil {
					c, _ := io.Copy(io.Discard, f)
					n += c
					f.Close()
				}
			}
		}
	} else {
		c, _ := io.Copy(io.Discard, r.Body)
		n += c
	}
	el := time.Since(start).Seconds()
	if el < 1e-9 {
		el = 1e-9
	}
	s.mu.Lock()
	s.UploadB = float64(n) / el
	s.mu.Unlock()
	http.Redirect(w, r, "/results?sid="+sid, http.StatusSeeOther)
}

// results: wait for download measurement (poll) up to timeout, then render
func results(w http.ResponseWriter, r *http.Request) {
	sid := r.URL.Query().Get("sid")
	if sid == "" {
		http.Error(w, "no sid", 400)
		return
	}
	s := idxGet(sid)

	waitTimeout := 30 * time.Second
	poll := 150 * time.Millisecond
	deadline := time.Now().Add(waitTimeout)

	for {
		s.mu.Lock()
		pings := append([]float64(nil), s.Pings...)
		download := s.DownloadB
		upload := s.UploadB
		client := s.ClientHost
		s.mu.Unlock()

		if download > 0 || time.Now().After(deadline) {
			if client == "" {
				client = getIP(r)
			}
			var avg, sd float64
			if len(pings) > 0 {
				for _, v := range pings {
					avg += v
				}
				avg /= float64(len(pings))
				for _, v := range pings {
					sd += (v - avg) * (v - avg)
				}
				sd = math.Sqrt(sd / float64(len(pings)))
			}
			io.WriteString(w, `<!doctype html><html><head><meta charset="utf-8"><title>results</title></head><body>
			<h3>Results</h3><table>
			<tr><td>Client host</td><td>`+client+`</td></tr>
			<tr><td>Ping avg (ms)</td><td>`+strconv.FormatFloat(avg, 'f', 2, 64)+`</td></tr>
			<tr><td>Jitter (ms)</td><td>`+strconv.FormatFloat(sd, 'f', 2, 64)+`</td></tr>
			<tr><td>Download</td><td>`+strconv.FormatFloat(download/1024/1024, 'f', 2, 64)+` MiB/s</td></tr>
			<tr><td>Upload</td><td>`+strconv.FormatFloat(upload/1024/1024, 'f', 2, 64)+` MiB/s</td></tr>
			</table><hr>
			<form method="POST" action="/upload" enctype="multipart/form-data">
			<input type="hidden" name="sid" value="`+sid+`">Upload file for upload-speed test: <input type="file" name="f"><button>Upload</button>
			</form><p><a href="/">Run again</a></p></body></html>`)
			return
		}
		time.Sleep(poll)
	}
}

func main() {
	http.HandleFunc("/", root)
	http.HandleFunc("/start", start)
	http.HandleFunc("/probe", probe)
	http.HandleFunc("/download", download)
	http.HandleFunc("/upload", upload)
	http.HandleFunc("/results", results)
	log.Println("listening :8080")
	log.Fatal(http.ListenAndServe(":8080", nil))
}
