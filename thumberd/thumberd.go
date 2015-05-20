package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"net/http/fcgi"
	"net/url"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"encoding/json"
	//"io/ioutil"

	"github.com/ikait/go-thumber/thumbnail"
)

var local = flag.String("local", "", "serve as webserver, example: 0.0.0.0:8000")
var timeout = flag.Int("timeout", 3, "timeout for upstream HTTP requests, in seconds")
var show_version = flag.Bool("version", false, "show version and exit")
var faceapi_key = flag.String("key", "", "your face api subscription key, if use face recognition")

var client http.Client

var version string
var isEnabledFaceApi bool = false

const maxDimension = 65000
const maxPixels = 10000000

const faceApiUrl = "https://api.projectoxford.ai/face/v0/detections?analyzesAge=true&analyzesGender=true"

type attributes struct {
	Gender string
	Age    int
}

type faceRectangle struct {
	Top    int
	Left   int
	Width  int
	Height int
}

type data struct {
	FaceId        string
	FaceRectangle faceRectangle
	Attributes    attributes
}

type data_wrapper []data

var http_stats struct {
	received       int64
	inflight       int64
	ok             int64
	thumb_error    int64
	upstream_error int64
	arg_error      int64
	total_time_us  int64
}

func init() {
	runtime.GOMAXPROCS(runtime.NumCPU())
}

func errorServer(w http.ResponseWriter, r *http.Request) {
	http.Error(w, "404 Not Found", http.StatusNotFound)
}

func statusServer(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain")
	fmt.Fprintf(w, "version %s\n", version)
	fmt.Fprintf(w, "received %d\n", atomic.LoadInt64(&http_stats.received))
	fmt.Fprintf(w, "inflight %d\n", atomic.LoadInt64(&http_stats.inflight))
	fmt.Fprintf(w, "ok %d\n", atomic.LoadInt64(&http_stats.ok))
	fmt.Fprintf(w, "thumb_error %d\n", atomic.LoadInt64(&http_stats.thumb_error))
	fmt.Fprintf(w, "upstream_error %d\n", atomic.LoadInt64(&http_stats.upstream_error))
	fmt.Fprintf(w, "arg_error %d\n", atomic.LoadInt64(&http_stats.arg_error))
	fmt.Fprintf(w, "total_time_us %d\n", atomic.LoadInt64(&http_stats.total_time_us))
}

func thumbServer(w http.ResponseWriter, r *http.Request) {
	startTime := time.Now()
	defer func() {
		elapsed := int64(time.Now().Sub(startTime) / 1000)
		atomic.AddInt64(&http_stats.total_time_us, elapsed)
	}()

	atomic.AddInt64(&http_stats.received, 1)
	atomic.AddInt64(&http_stats.inflight, 1)
	defer atomic.AddInt64(&http_stats.inflight, -1)

	path := r.URL.RequestURI()

	// Defaults
	var params = thumbnail.ThumbnailParameters{
		Upscale:        true,
		ForceAspect:    true,
		Quality:        90,
		Optimize:       false,
		PrescaleFactor: 2.0,
	}

	if path[0] != '/' {
		http.Error(w, "Path should start with /", http.StatusBadRequest)
		atomic.AddInt64(&http_stats.arg_error, 1)
		return
	}
	parts := strings.SplitN(path[1:], "/", 2)
	if len(parts) < 2 {
		http.Error(w, "Path needs to have at least two components", http.StatusBadRequest)
		atomic.AddInt64(&http_stats.arg_error, 1)
		return
	}
	for _, arg := range strings.Split(parts[0], ",") {
		tup := strings.SplitN(arg, "=", 2)
		if len(tup) != 2 {
			http.Error(w, "Arguments must have the form name=value", http.StatusBadRequest)
			atomic.AddInt64(&http_stats.arg_error, 1)
			return
		}
		switch tup[0] {
		case "w", "h", "q", "u", "a", "o", "f":
			val, err := strconv.Atoi(tup[1])
			if err != nil {
				http.Error(w, "Invalid integer value for "+tup[0], http.StatusBadRequest)
				atomic.AddInt64(&http_stats.arg_error, 1)
				return
			}
			switch tup[0] {
			case "w":
				params.Width = val
			case "h":
				params.Height = val
			case "q":
				params.Quality = val
			case "u":
				params.Upscale = val != 0
			case "a":
				params.ForceAspect = val != 0
			case "o":
				params.Optimize = val != 0
			case "f":
				isEnabledFaceApi = val != 0
			}
		case "p":
			val, err := strconv.ParseFloat(tup[1], 64)
			if err != nil {
				http.Error(w, "Invalid float value for "+tup[0], http.StatusBadRequest)
				atomic.AddInt64(&http_stats.arg_error, 1)
				return
			}
			params.PrescaleFactor = val
		}
	}
	if params.Width <= 0 || params.Width > maxDimension {
		http.Error(w, "Width (w) not specified or invalid", http.StatusBadRequest)
		atomic.AddInt64(&http_stats.arg_error, 1)
		return
	}
	if params.Height <= 0 || params.Height > maxDimension {
		http.Error(w, "Height (h) not specified or invalid", http.StatusBadRequest)
		atomic.AddInt64(&http_stats.arg_error, 1)
		return
	}
	if params.Width*params.Height > maxPixels {
		http.Error(w, "Image dimensions are insane", http.StatusBadRequest)
		atomic.AddInt64(&http_stats.arg_error, 1)
		return
	}
	if params.Quality > 100 || params.Quality < 0 {
		http.Error(w, "Quality must be between 0 and 100", http.StatusBadRequest)
		atomic.AddInt64(&http_stats.arg_error, 1)
		return
	}

	if isEnabledFaceApi {
		w.Header().Set("X-Face-On", "1")
		jsonString := "{\"url\": \"http://" + parts[1] + "\"}"
		req, err := http.NewRequest("POST", faceApiUrl, strings.NewReader(jsonString))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Ocp-Apim-Subscription-Key", *faceapi_key)
		res, err := http.DefaultClient.Do(req)

		if err == nil {
			w.Header().Set("X-Face-HTTP-Status", res.Status)
			w.Header().Set("X-Face-HTTP-Status-Code", strconv.Itoa(res.StatusCode))
			if res.StatusCode == http.StatusOK {
				dec := json.NewDecoder(res.Body)
				var d data_wrapper
				dec.Decode(&d)
				if len(d) != 0 {
					data := d[0]
					fmt.Printf("%+v\n", "http://"+parts[1])
					fmt.Printf("%+v\n", data)

					w.Header().Set("X-Face-Contains", "1")
					w.Header().Set("X-Face-FaceRectangle-Top", strconv.Itoa(data.FaceRectangle.Top))
					w.Header().Set("X-Face-FaceRectangle-Left", strconv.Itoa(data.FaceRectangle.Left))
					w.Header().Set("X-Face-FaceRectangle-Width", strconv.Itoa(data.FaceRectangle.Width))
					w.Header().Set("X-Face-FaceRectangle-Height", strconv.Itoa(data.FaceRectangle.Height))

					w.Header().Set("X-Face-Attributes-Gender", data.Attributes.Gender)
					w.Header().Set("X-Face-Attributes-Age", strconv.Itoa(data.Attributes.Age))
				} else {
					w.Header().Set("X-Face-On", "1")
					w.Header().Set("X-Face-Contains", "0")
				}

			} else {

			}
		} else {
			println("something wrong")
		}
		res.Body.Close()
	} else {
		w.Header().Set("X-Face-On", "0")
	}

	srcReader, err := http.DefaultClient.Get("http://" + parts[1])
	if err != nil {
		http.Error(w, "Upstream failed: "+err.Error(), http.StatusBadGateway)
		atomic.AddInt64(&http_stats.upstream_error, 1)
		return
	}
	if srcReader.StatusCode != http.StatusOK {
		http.Error(w, "Upstream failed: "+srcReader.Status, srcReader.StatusCode)
		atomic.AddInt64(&http_stats.upstream_error, 1)
		return
	}

	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Last-Modified", time.Now().UTC().Format(http.TimeFormat))
	err = thumbnail.MakeThumbnail(srcReader.Body, w, params)
	if err != nil {
		switch err := err.(type) {
		case *url.Error:
			http.Error(w, "Upstream failed: "+err.Error(), http.StatusBadGateway)
			atomic.AddInt64(&http_stats.upstream_error, 1)
			return
		default:
			println(srcReader.StatusCode)
			http.Error(w, "Thumbnailing failed: "+err.Error(), http.StatusInternalServerError)
			atomic.AddInt64(&http_stats.thumb_error, 1)
			return
		}
	}
	srcReader.Body.Close()
	atomic.AddInt64(&http_stats.ok, 1)
}

func main() {
	flag.Parse()
	if *show_version {
		fmt.Printf("thumberd %s\n", version)
		return
	}

	client.Timeout = time.Duration(*timeout) * time.Second

	var err error

	http.HandleFunc("/server-status", statusServer)
	http.HandleFunc("/favicon.ico", errorServer)

	http.HandleFunc("/", thumbServer)

	if *local != "" { // Run as a local web server
		err = http.ListenAndServe(*local, nil)
	} else { // Run as FCGI via standard I/O
		err = fcgi.Serve(nil, nil)
	}
	if err != nil {
		log.Fatal(err)
	}
}
