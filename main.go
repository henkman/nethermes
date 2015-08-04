package main

import (
	"archive/zip"
	"code.google.com/p/log4go"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/gorilla/mux"
	"html/template"
	"io"
	"math/rand"
	"mime/multipart"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"time"
)

const (
	KEY_TRIES = 3
)

var (
	transfers     = map[string]*Transfer{}
	indextemplate *template.Template
	conf          Config
	logger        log4go.Logger
)

type Config struct {
	KeyCharset     string
	KeyLength      int
	Port           int
	TimeoutMinutes int
	CheckMinutes   int
}

type Status uint8

const (
	WAIT Status = iota
	INPROGRESS
	TIMEOUT
	DONE
)

type Transfer struct {
	Mr     *multipart.Reader
	Status Status
}

func GenerateUniqueKey() (string, error) {
	for i := 0; i < KEY_TRIES; i++ {
		key := GenerateKey()
		if _, ok := transfers[key]; !ok {
			return key, nil
		}
	}

	return "", errors.New("no unique key found")
}

func GenerateKey() string {
	key := make([]byte, conf.KeyLength)
	for i := 0; i < conf.KeyLength; i++ {
		r := rand.Int31n(int32(len(conf.KeyCharset)))
		key[i] = conf.KeyCharset[r : r+1][0]
	}

	return string(key)
}

func StatusHandler(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	id := vars["id"]

	transfer, exists := transfers[id]
	if !exists {
		http.Error(w, "transfer does not exist", http.StatusBadRequest)
		return
	}

	w.Header().Set("Content-Type", "text/javascript")
	jenc := json.NewEncoder(w)
	jenc.Encode(transfer.Status)
}

func UploadHandler(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	id := vars["id"]

	if _, exists := transfers[id]; exists {
		http.Error(w, "internal error", http.StatusBadRequest)
		return
	}

	mr, err := r.MultipartReader()
	if err != nil {
		http.Error(w, "internal error", http.StatusBadRequest)
		return
	}

	transfer := &Transfer{
		mr,
		WAIT,
	}
	transfers[id] = transfer

	timeout := time.After(time.Minute * time.Duration(conf.TimeoutMinutes))
	for transfer.Status == WAIT {
		select {
		case <-timeout:
			http.Error(w, "no receiver found", http.StatusBadRequest)
			transfer.Status = TIMEOUT
		}
	}
	w.Write([]byte("ok"))
}

func DownloadHandler(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	id := vars["id"]

	transfer, exists := transfers[id]
	if !exists || transfer.Status != WAIT {
		http.Error(w, "transfer does not exist", http.StatusBadRequest)
		return
	}

	w.Header().Set("Content-Disposition", "attachment; filename="+id+".zip")
	transfer.Status = INPROGRESS
	zout := zip.NewWriter(w)
	defer zout.Close()
	for {
		p, err := transfer.Mr.NextPart()
		if err == io.EOF {
			break
		}

		if p.FormName() == "file" {
			out, _ := zout.Create(p.FileName())
			io.Copy(out, p)
		}
		p.Close()
	}
	transfer.Status = DONE
}

func IndexHandler(w http.ResponseWriter, r *http.Request) {
	key, err := GenerateUniqueKey()
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	w.Header().Set("Content-Type", "text/html")
	indextemplate.Execute(w, struct {
		Key  string
		Host string
	}{
		key,
		r.Host,
	})
}

func ReadConfig(file string) (Config, error) {
	conf := Config{
		Port:           8080,
		TimeoutMinutes: 3,
		KeyCharset:     "abcdefghijklmnopqrstuvwxyz0123456789",
		KeyLength:      10,
		CheckMinutes:   3,
	}
	fd, err := os.Open(file)
	if err != nil {
		return conf, err
	}
	defer fd.Close()

	jdec := json.NewDecoder(fd)
	err = jdec.Decode(&conf)
	return conf, err
}

func CleanOld() {
	clean := func() {
		for id, transfer := range transfers {
			if transfer.Status == TIMEOUT || transfer.Status == DONE {
				delete(transfers, id)
			}
		}
	}

	t := time.NewTicker(time.Minute * time.Duration(conf.CheckMinutes))
	for {
		select {
		case <-t.C:
			clean()
		}
	}
}

func Log(handler http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		logger.Info("%s %s %s", r.RemoteAddr, r.Method, r.URL)
		handler.ServeHTTP(w, r)
	})
}

func init() {
	runtime.GOMAXPROCS(runtime.NumCPU())

	logger = make(log4go.Logger)
	flw := log4go.NewFileLogWriter("./log/http.log", true)
	flw.SetFormat("[%D %T] [%L] %M")
	flw.SetRotateSize(1024 * 1024 * 1024)
	logger.AddFilter("file", log4go.INFO, flw)

	var err error

	conf, err = ReadConfig("./nethermes.json")
	if err != nil {
		logger.Info("Could not read nethermes.json")
	}
	logger.Info("Using following configuration: %+v", conf)

	idRegex := fmt.Sprintf("[%s]{%d}", conf.KeyCharset, conf.KeyLength)

	rand.Seed(time.Now().Unix() + 3301)
	r := mux.NewRouter()
	s := r.Methods("GET").Subrouter()
	s.HandleFunc("/", IndexHandler)
	s.HandleFunc("/status/{id:"+idRegex+"}", StatusHandler)
	s.HandleFunc("/download/{id:"+idRegex+"}", DownloadHandler)
	s.Handle("/{_:(.*)}", http.FileServer(http.Dir("./htdocs")))
	s = r.Methods("POST").Subrouter()
	s.HandleFunc("/upload/{id:"+idRegex+"}", UploadHandler)
	http.Handle("/", r)

	indextemplate, err = template.ParseFiles("./index.html")
	if err != nil {
		logger.Critical("Parse template: ", err)
		os.Exit(1)
	}
	go CleanOld()
}

func main() {
	port := strconv.Itoa(conf.Port)
	err := http.ListenAndServe(":"+port, Log(http.DefaultServeMux))
	if err != nil {
		logger.Critical(err)
		os.Exit(1)
	}
}
