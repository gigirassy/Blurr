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
	<p>Download test serves ~4MiB; upload uses a file you select.</p>
	</body></html>`)
}

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
	http.Redirect(w, r, "/probe?sid="+sid+"&n=1", http.StatusFound)
}

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
		http.Redirect(w, r, "/probe?sid="+sid+"&n="+strconv.Itoa(next), http.StatusFound)
		return
	}

	// final probe -> render a page that contains a non-cached, nonce'd image URL which will force a new HTTP fetch
	nonce := strconv.FormatInt(time.Now().UnixNano(), 36)
	size := 8 * 1024 * 1024
	html := `<!doctype html><html><head><meta charset="utf-8"><title>download</title></head><body>
	<h3>Starting download test</h3>
	<!-- nonce prevents caching; browser will fetch the resource -->
	<img src="/download?sid=` + sid + `&size=` + strconv.Itoa(size) + `&nonce=` + nonce + `" style="display:none">
	<p>If the image doesn't start automatically, click <a href="/download?sid=` + sid + `&size=` + strconv.Itoa(size) + `&nonce=` + nonce + `">here</a> to start it.</p>
	<p>Results will appear after the server finishes the download.</p>
	</body></html>`
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write([]byte(html))
}

func download(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	sid := q.Get("sid")
	size, _ := strconv.Atoi(q.Get("size"))
	if size <= 0 {
		size = 8 * 1024 * 1024 // default 8MiB
	}

	// prevent caching by browsers / proxies
	w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate, max-age=0")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Expires", "0")
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

	// store measurement
	if sid != "" {
		s := idxGet(sid)
		s.mu.Lock()
		s.DownloadB = bps
		s.mu.Unlock()
	}

	// log server-side result for debugging
	log.Printf("download done sid=%s bytes=%d elapsed=%.3fs bps=%.2fMB/s\n", sid, bw, elapsed, bps/1024/1024)
}

func upload(w http.ResponseWriter, r *http.Request) {
	// accept multipart/form-data file upload
	if err := r.ParseMultipartForm(32 << 20); err != nil && err != http.ErrNotMultipart {
		// still attempt to read body
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

func results(w http.ResponseWriter, r *http.Request) {
	sid := r.URL.Query().Get("sid")
	if sid == "" {
		http.Error(w, "no sid", 400)
		return
	}
	s := idxGet(sid)

	// wait up to waitTimeout for download to complete (i.e., s.DownloadB > 0)
	waitTimeout := 20 * time.Second
	poll := 100 * time.Millisecond
	deadline := time.Now().Add(waitTimeout)
	for {
		s.mu.Lock()
		download := s.DownloadB
		upload := s.UploadB
		pings := append([]float64(nil), s.Pings...)
		client := s.ClientHost
		s.mu.Unlock()

		// if we have a download measurement or timed out, render results
		if download > 0 || time.Now().After(deadline) {
			if client == "" {
				client = getIP(r)
			}
			// compute avg & jitter
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
		// otherwise sleep a bit and poll again
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
