package main

import (
	"io"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

func getIP(r *http.Request) string {
	if x := r.Header.Get("X-Forwarded-For"); x != "" {
		if i := strings.IndexByte(x, ','); i >= 0 {
			return strings.TrimSpace(x[:i])
		}
		return x
	}
	h, _, _ := net.SplitHostPort(r.RemoteAddr)
	return h
}

func root(w http.ResponseWriter, r *http.Request) {
	ip := getIP(r)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	io.WriteString(w, `<!doctype html>
<html><head><meta charset="utf-8"><title>Blurr (JS primary)</title>
<style>body{font-family:system-ui,Segoe UI,Roboto,Arial;max-width:760px;margin:1rem}</style>
</head><body>
<h2>Blurr</h2>
<p>Host: `+ip+`</p>
<div id=out>Click <button id=start>Start test</button> to run. JS required for automatic test; no-JS fallback links below.</div>

<pre id=log style="background:#f6f6f6;padding:.5rem"></pre>

<!-- no-JS fallback -->
<noscript>
  <p><strong>No JavaScript detected.</strong></p>
  <p>Manual tests:</p>
  <ul>
    <li><a href="/download?size=8388608&nonce=manual">Download 8MiB</a> — click to fetch</li>
    <li>Upload: POST a file to <code>/upload</code> with a form or curl</li>
    <li>Ping: use <code>curl -w "%{time_starttransfer}\\n" -o /dev/null /ping</code></li>
  </ul>
</noscript>

<script>
const $ = id=>document.getElementById(id);
function log(s){ $("log").textContent += s+"\n" }
async function pingRuns(n=6){
  const times=[];
  for(let i=0;i<n;i++){
    const t0=performance.now();
    await fetch('/ping?nonce='+Date.now(),{cache:'no-store',headers:{"x-ts":"1"}});
    const t1=performance.now();
    times.push(t1-t0);
    await new Promise(r=>setTimeout(r,80));
  }
  return times;
}
function stats(arr){
  const sum=arr.reduce((a,b)=>a+b,0);
  const avg=sum/arr.length;
  let sd=0;
  for(const v of arr) sd += (v-avg)*(v-avg);
  sd = Math.sqrt(sd/arr.length);
  return {avg,sd};
}
async function downloadTest(size=8*1024*1024){
  const url='/download?size='+size+'&nonce='+Date.now();
  const res = await fetch(url,{cache:'no-store'});
  if(!res.body) throw "no stream";
  const reader = res.body.getReader();
  let seen=0;
  const t0=performance.now();
  while(true){
    const {done,value} = await reader.read();
    if(done) break;
    seen += value.byteLength;
  }
  const t1=performance.now();
  const secs=(t1-t0)/1000;
  return {bps: seen/secs, bytes:seen, secs};
}
function uploadTest(size=8*1024*1024){
  return new Promise((resolve,reject)=>{
    const xhr=new XMLHttpRequest();
    const url='/upload?nonce='+Date.now();
    xhr.open('POST',url);
    const start=performance.now();
    xhr.onload = ()=>{
      const secs = (performance.now()-start)/1000;
      resolve({secs, bps: size/secs});
    };
    xhr.onerror = ()=>reject("upload error");
    // make buffer (small memory pressure for typical sizes)
    const arr=new Uint8Array(size);
    arr.fill(97);
    xhr.send(arr.buffer);
  });
}

$("start").onclick = async ()=>{
  $("start").disabled = true;
  log("Starting ping...");
  try{
    const pings = await pingRuns();
    const s = stats(pings);
    log("Ping avg (ms): "+s.avg.toFixed(2));
    log("Jitter (ms): "+s.sd.toFixed(2));
    log("Starting download (streamed)...");
    const d = await downloadTest();
    log("Download: "+(d.bps/1024/1024).toFixed(2)+" MiB/s ("+d.bytes+" bytes in "+d.secs.toFixed(2)+"s)");
    log("Starting upload (XHR)...");
    const u = await uploadTest();
    log("Upload: "+(u.bps/1024/1024).toFixed(2)+" MiB/s ("+u.secs.toFixed(2)+"s)");
    log("Done.");
  }catch(e){
    log("Error: "+e);
  } finally {
    $("start").disabled = false;
  }
};
</script>
<span>Donations are not needed. Instead, <a href="https://github.com/gigirassy/Blurr/">consider contributing to the CC0 code</a>.</span>
</body></html>`)
}

func ping(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate, max-age=0")
	w.Header().Set("Content-Type", "text/plain")
	w.Write([]byte("1"))
}

func download(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	size, _ := strconv.Atoi(q.Get("size"))
	if size <= 0 {
		size = 8 * 1024 * 1024
	}
	w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate, max-age=0")
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.Itoa(size))
	chunk := make([]byte, 32*1024)
	for i := range chunk {
		chunk[i] = 'a'
	}
	bw := 0
	start := time.Now()
	fl, _ := w.(http.Flusher)
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
		if fl != nil {
			fl.Flush()
		}
	}
	elapsed := time.Since(start).Seconds()
	if elapsed < 1e-9 {
		elapsed = 1e-9
	}
	log.Printf("download done bytes=%d elapsed=%.3f bps=%.3fMiB/s\n", bw, elapsed, float64(bw)/1024.0/1024.0/elapsed)
}

func upload(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	var n int64
	n, _ = io.Copy(io.Discard, r.Body)
	el := time.Since(start).Seconds()
	if el < 1e-9 {
		el = 1e-9
	}
	log.Printf("upload received bytes=%d elapsed=%.3f bps=%.3fMiB/s\n", n, el, float64(n)/1024.0/1024.0/el)
	w.Write([]byte("ok"))
}

func main() {
	http.HandleFunc("/", root)
	http.HandleFunc("/ping", ping)
	http.HandleFunc("/download", download)
	http.HandleFunc("/upload", upload)
	log.Println("listening :8080")
	log.Fatal(http.ListenAndServe(":8080", nil))
}
