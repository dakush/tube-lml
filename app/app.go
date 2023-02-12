// Package app manages main application server.
package app

import (
	"fmt"
	"html/template"
	"io"
	"io/ioutil"
	"net"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"git.mills.io/prologic/tube/app/middleware"
	"git.mills.io/prologic/tube/importers"
	"git.mills.io/prologic/tube/media"
	"git.mills.io/prologic/tube/static"
	"git.mills.io/prologic/tube/templates"
	"git.mills.io/prologic/tube/utils"

	"github.com/cyphar/filepath-securejoin"
	"github.com/dustin/go-humanize"
	"github.com/fsnotify/fsnotify"
	"github.com/gorilla/handlers"
	"github.com/gorilla/mux"
	shortuuid "github.com/lithammer/shortuuid/v3"
	log "github.com/sirupsen/logrus"
)

// App represents main application.
type App struct {
	Config    *Config
	Library   *media.Library
	Store     Store
	Watcher   *fsnotify.Watcher
	Templates *templateStore
	Feed      []byte
	Listener  net.Listener
	Router    *mux.Router
}

// NewApp returns a new instance of App from Config.
func NewApp(cfg *Config) (*App, error) {
	if cfg == nil {
		cfg = DefaultConfig()
	}
	a := &App{
		Config: cfg,
	}
	// Setup Library
	a.Library = media.NewLibrary()
	// Setup Store
	store, err := NewBitcaskStore(cfg.Server.StorePath)
	if err != nil {
		err := fmt.Errorf("error opening store %s: %w", cfg.Server.StorePath, err)
		return nil, err
	}
	a.Store = store
	// Setup Watcher
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	a.Watcher = w
	// Setup Listener
	ln, err := newListener(cfg.Server)
	if err != nil {
		return nil, err
	}
	a.Listener = ln

	// Templates

	a.Templates = newTemplateStore("base")

	templateFuncs := map[string]interface{}{
		"bytes": func(size int64) string { return humanize.Bytes(uint64(size)) },
	}

	indexTemplate := template.New("index").Funcs(templateFuncs)
	template.Must(indexTemplate.Parse(templates.MustGetTemplate("index.html")))
	template.Must(indexTemplate.Parse(templates.MustGetTemplate("base.html")))
	a.Templates.Add("index", indexTemplate)

	uploadTemplate := template.New("upload").Funcs(templateFuncs)
	template.Must(uploadTemplate.Parse(templates.MustGetTemplate("upload.html")))
	template.Must(uploadTemplate.Parse(templates.MustGetTemplate("base.html")))
	a.Templates.Add("upload", uploadTemplate)

	importTemplate := template.New("import").Funcs(templateFuncs)
	template.Must(importTemplate.Parse(templates.MustGetTemplate("import.html")))
	template.Must(importTemplate.Parse(templates.MustGetTemplate("base.html")))
	a.Templates.Add("import", importTemplate)

	// Setup Router
	authPassword := os.Getenv("auth_password")
	isSandstorm := os.Getenv("SANDSTORM")

	r := mux.NewRouter().StrictSlash(true)
	r.HandleFunc("/", a.indexHandler).Methods("GET", "OPTIONS")
	if isSandstorm == "1" {
		r.HandleFunc("/upload", middleware.RequireSandstormPermission(a.uploadHandler, "upload")).Methods("GET", "OPTIONS", "POST")
	} else {
		r.HandleFunc("/upload", middleware.OptionallyRequireAdminAuth(a.uploadHandler, authPassword)).Methods("GET", "OPTIONS", "POST")
	}
	r.HandleFunc("/import", a.importHandler).Methods("GET", "OPTIONS", "POST")
	r.HandleFunc("/v/{id}.mp4", a.videoHandler).Methods("GET")
	r.HandleFunc("/v/{prefix}/{id}.mp4", a.videoHandler).Methods("GET")
	r.HandleFunc("/t/{id}", a.thumbHandler).Methods("GET")
	r.HandleFunc("/t/{prefix}/{id}", a.thumbHandler).Methods("GET")
	r.HandleFunc("/v/{id}", a.pageHandler).Methods("GET")
	r.HandleFunc("/v/{prefix}/{id}", a.pageHandler).Methods("GET")
	r.HandleFunc("/feed.xml", a.rssHandler).Methods("GET")
	// Static file handler
	fsHandler := http.StripPrefix(
		"/static",
		http.FileServer(static.GetFilesystem()),
	)
	r.PathPrefix("/static/").Handler(fsHandler).Methods("GET")

	cors := handlers.CORS(
		handlers.AllowedHeaders([]string{
			"X-Requested-With",
			"Content-Type",
			"Authorization",
		}),
		handlers.AllowedMethods([]string{
			"GET",
			"POST",
			"PUT",
			"HEAD",
			"OPTIONS",
		}),
		handlers.AllowedOrigins([]string{"*"}),
		handlers.AllowCredentials(),
	)

	r.Use(cors)

	a.Router = r
	return a, nil
}

// Run imports the library and starts server.
func (a *App) Run() error {
	for _, pc := range a.Config.Library {
		pc.Path = filepath.Clean(pc.Path)
		p := &media.Path{
			Path:                   pc.Path,
			Prefix:                 pc.Prefix,
			PreserveUploadFilename: pc.PreserveUploadFilename,
		}
		err := a.Library.AddPath(p)
		if err != nil {
			return err
		}
		err = a.Library.Import(p)
		if err != nil {
			return err
		}
		a.Watcher.Add(p.Path)
	}
	if _, err := os.Stat(a.Config.Server.UploadPath) ; err != nil && os.IsNotExist(err) {
		log.Warn(
			fmt.Sprintf("app: upload path '%s' does not exist. Creating it now.",
			a.Config.Server.UploadPath))
		if err := os.MkdirAll(a.Config.Server.UploadPath, 0o755); err != nil {
			return fmt.Errorf(
				"error creating upload path %s: %w",
				a.Config.Server.UploadPath, err)
		}
	}
	buildFeed(a)
	go startWatcher(a)
	return http.Serve(a.Listener, a.Router)
}

func (a *App) render(name string, w http.ResponseWriter, ctx interface{}) {
	buf, err := a.Templates.Exec(name, ctx)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}

	_, err = buf.WriteTo(w)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// HTTP handler for /
func (a *App) indexHandler(w http.ResponseWriter, r *http.Request) {
	log.Printf("/")
	pl := a.Library.Playlist()
	if len(pl) > 0 {
		http.Redirect(w, r, fmt.Sprintf("/v/%s?%s", pl[0].ID, r.URL.RawQuery), 302)
	} else {
		sort := strings.ToLower(r.URL.Query().Get("sort"))
		quality := strings.ToLower(r.URL.Query().Get("quality"))
		ctx := &struct {
			Sort     string
			Quality  string
			Config   *Config
			Playing  *media.Video
			Playlist media.Playlist
		}{
			Sort:     sort,
			Quality:  quality,
			Config:   a.Config,
			Playing:  &media.Video{ID: ""},
			Playlist: a.Library.Playlist(),
		}

		a.render("index", w, ctx)
	}
}

func filenameWithoutExtension(path string) (stem string) {
	var basename string = filepath.Base(path)
	return basename[0:len(basename)-len(filepath.Ext(basename))]
}

// Render the upload page where clients can select and upload their video files
func (a *App) renderUploadPage(respWriter http.ResponseWriter) {
	ctx := &struct {
		Config  *Config
		Playing *media.Video
	}{
		Config:  a.Config,
		Playing: &media.Video{ID: ""},
	}
	a.render("upload", respWriter, ctx)
}

// HTTP handler for /upload
func (a *App) uploadHandler(respWriter http.ResponseWriter, request *http.Request) {
	if request.Method == "GET" {
		a.renderUploadPage(respWriter)
	} else if request.Method == "POST" {

		// The number here is probably used in the wrong place.
		// Why should we expect tube to fit all uploads into memory?
		request.ParseMultipartForm(a.Config.Server.MaxUploadSize)

		// get information from the upload form and make sure it is valid
		videoTitleFromUpload := request.FormValue("video_title")
		videoDescriptionFromUpload := request.FormValue("video_description")
		if _, exists := a.Library.Paths[request.FormValue("target_library_path")]; !exists {
			err := fmt.Errorf("uploading to invalid library path: %s", request.FormValue("target_library_path"))
			log.Error(err)
			return
		}
		targetLibraryPath := request.FormValue("target_library_path")

		// save uploaded data in upload directory
		fileContentFromUpload, fileHeaderFromUpload, err := request.FormFile("video_file")
		if err != nil {
			err := fmt.Errorf("error processing form: %w", err)
			log.Error(err)
			http.Error(respWriter, err.Error(), http.StatusInternalServerError)
			return
		}
		defer fileContentFromUpload.Close()

		// keeping the file extension from the upload file probably makes it easier for ffmpeg to
		// read the file for transcoding later
		uploadedFile, err := ioutil.TempFile(
			a.Config.Server.UploadPath,
			fmt.Sprintf("tube-upload-*%s", filepath.Ext(fileHeaderFromUpload.Filename)),
		)
		if err != nil {
			err := fmt.Errorf("error creating temporary file for uploading: %w", err)
			log.Error(err)
			http.Error(respWriter, err.Error(), http.StatusInternalServerError)
			return
		}
		defer os.Remove(uploadedFile.Name())

		_, err = io.Copy(uploadedFile, fileContentFromUpload)
		if err != nil {
			err := fmt.Errorf("error writing file: %w", err)
			log.Error(err)
			http.Error(respWriter, err.Error(), http.StatusInternalServerError)
			return
		}


		// create temporary file for transcoded video file
		temporaryTranscodedFile, err := ioutil.TempFile(
			a.Config.Server.UploadPath,
			fmt.Sprintf("tube-transcode-*.mp4"),
		)
		if err != nil {
			err := fmt.Errorf("error creating temporary file for transcoding: %w", err)
			log.Error(err)
			http.Error(respWriter, err.Error(), http.StatusInternalServerError)
			return
		}

		// Here we set the final filename for the video file after transcoding.
		var newVideoAbsolutePath string

		if a.Config.Server.PreserveUploadFilename ||
		   a.Library.Paths[targetLibraryPath].PreserveUploadFilename {
			newVideoAbsolutePath, err = securejoin.SecureJoin(
				a.Library.Paths[targetLibraryPath].Path,
				fmt.Sprintf("%s.mp4", filenameWithoutExtension(fileHeaderFromUpload.Filename)),
			)
		} else {
			newVideoAbsolutePath, err = securejoin.SecureJoin(
				a.Library.Paths[targetLibraryPath].Path,
				fmt.Sprintf("%s.mp4", shortuuid.New()),
			)
		}
		if err != nil {
			err := fmt.Errorf("error creating file name in target library: %w", err)
			log.Error(err)
			http.Error(respWriter, err.Error(), http.StatusInternalServerError)
			return
		}
		// If the (sanitized) original filename collides with an existing file,
		// we try to add a shortuuid() to it until we find one that doesn't exist.
		for _, err := os.Stat(newVideoAbsolutePath) ; ! os.IsNotExist(err) ; _, err = os.Stat(newVideoAbsolutePath) {
			if err != nil {
				log.Error(err)
				http.Error(respWriter, err.Error(), http.StatusInternalServerError)
				return
			}
			log.Warn("File '"+ newVideoAbsolutePath + "' already exists.");
			newVideoAbsolutePath, err = securejoin.SecureJoin(
				a.Library.Paths[targetLibraryPath].Path,
				fmt.Sprintf("%s_%s.mp4", filenameWithoutExtension(newVideoAbsolutePath), shortuuid.New()),
			)
			if err != nil {
				err := fmt.Errorf("error creating file name in target library: %w", err)
				log.Error(err)
				http.Error(respWriter, err.Error(), http.StatusInternalServerError)
				return
			}
			log.Warn("Using filename '" + newVideoAbsolutePath + "' instead.");
		}

		temporaryTranscodedFileThumbnailPath := fmt.Sprintf("%s.jpg", strings.TrimSuffix(temporaryTranscodedFile.Name(), filepath.Ext(temporaryTranscodedFile.Name())))
		newVideoAbsoluteThumbnailPath := fmt.Sprintf("%s.jpg", strings.TrimSuffix(newVideoAbsolutePath, filepath.Ext(newVideoAbsolutePath)))


		// run the transcoder
		// TODO: Use a proper Job Queue and make this async
		if err := utils.RunCmd(
			a.Config.Transcoder.Timeout,
			"ffmpeg",
			"-y",
			"-i", uploadedFile.Name(),
			"-vcodec", "h264",
			"-acodec", "aac",
			"-strict", "-2",
			"-loglevel", "quiet",
			"-metadata", fmt.Sprintf("title=%s", videoTitleFromUpload),
			"-metadata", fmt.Sprintf("comment=%s", videoDescriptionFromUpload),
			temporaryTranscodedFile.Name(),
		); err != nil {
			err := fmt.Errorf("error transcoding video: %w", err)
			log.Error(err)
			http.Error(respWriter, err.Error(), http.StatusInternalServerError)
			return
		}

		// Create the thumbnail
		if err := utils.RunCmd(
			a.Config.Thumbnailer.Timeout,
			"ffmpeg",
			"-i", uploadedFile.Name(),
			"-y",
			"-vf", "thumbnail",
			"-t", fmt.Sprint(a.Config.Thumbnailer.PositionFromStart),
			"-vframes", "1",
			"-strict", "-2",
			"-loglevel", "quiet",
			temporaryTranscodedFileThumbnailPath,
		); err != nil {
			err := fmt.Errorf("error generating thumbnail: %w", err)
			log.Error(err)
			http.Error(respWriter, err.Error(), http.StatusInternalServerError)
			return
		}

		// move transcoded video file and the thumbnail to its final destination
		// in the library. move thumbnail first, so that a thumbnail is found
		// when the library path watcher triggers the addition of that new file
		if err := os.Rename(temporaryTranscodedFileThumbnailPath, newVideoAbsoluteThumbnailPath); err != nil {
			err := fmt.Errorf("error renaming generated thumbnail: %w", err)
			log.Error(err)
			http.Error(respWriter, err.Error(), http.StatusInternalServerError)
			return
		}
		if err := os.Rename(temporaryTranscodedFile.Name(), newVideoAbsolutePath); err != nil {
			err := fmt.Errorf("error renaming transcoded video: %w", err)
			log.Error(err)
			http.Error(respWriter, err.Error(), http.StatusInternalServerError)
			return
		}

		// TODO: Make this a background job
		// Resize for lower quality options
		// this seems to be working in the library directly, instead of in the upload directory
		// so this will trigger a heck of a lot imports until it is done.
		// scaled transcoded files should definitely be written to the upload directory
		// and only be moved to the library shelf when they are done.
		for size, suffix := range a.Config.Transcoder.Sizes {
			log.
				WithField("size", size).
				WithField("vf", filepath.Base(newVideoAbsolutePath)).
				Info("resizing video for lower quality playback")
			sf := fmt.Sprintf(
				"%s#%s.mp4",
				strings.TrimSuffix(newVideoAbsolutePath, filepath.Ext(newVideoAbsolutePath)),
				suffix,
			)

			if err := utils.RunCmd(
				a.Config.Transcoder.Timeout,
				"ffmpeg",
				"-y",
				"-i", newVideoAbsolutePath,
				"-s", size,
				"-c:v", "libx264",
				"-c:a", "aac",
				"-crf", "18",
				"-strict", "-2",
				"-loglevel", "quiet",
				"-metadata", fmt.Sprintf("title=%s", videoTitleFromUpload),
				"-metadata", fmt.Sprintf("comment=%s", videoDescriptionFromUpload),
				sf,
			); err != nil {
				err := fmt.Errorf("error transcoding video: %w", err)
				log.Error(err)
				http.Error(respWriter, err.Error(), http.StatusInternalServerError)
				return
			}
		}

		fmt.Fprintf(respWriter, "Video successfully uploaded!")
	} else {
		http.Error(respWriter, "Method Not Allowed", http.StatusMethodNotAllowed)
	}
}

// HTTP handler for /import
func (a *App) importHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method == "GET" {
		ctx := &struct {
			Config  *Config
			Playing *media.Video
		}{
			Config:  a.Config,
			Playing: &media.Video{ID: ""},
		}
		a.render("import", w, ctx)
	} else if r.Method == "POST" {
		r.ParseMultipartForm(1024)

		url := r.FormValue("url")
		if url == "" {
			err := fmt.Errorf("error, no url supplied")
			log.Error(err)
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		// TODO: Make collection user selectable from drop-down in Form
		// XXX: Assume we can put uploaded videos into the first collection (sorted) we find
		keys := make([]string, 0, len(a.Library.Paths))
		for k := range a.Library.Paths {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		collection := keys[0]

		videoImporter, err := importers.NewImporter(url)
		if err != nil {
			err := fmt.Errorf("error creating video importer for %s: %w", url, err)
			log.Error(err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		videoInfo, err := videoImporter.GetVideoInfo(url)
		if err != nil {
			err := fmt.Errorf("error retriving video info for %s: %w", url, err)
			log.Error(err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		uf, err := ioutil.TempFile(
			a.Config.Server.UploadPath,
			fmt.Sprintf("tube-import-*.mp4"),
		)
		if err != nil {
			err := fmt.Errorf("error creating temporary file for importing: %w", err)
			log.Error(err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		defer os.Remove(uf.Name())

		log.WithField("video_url", videoInfo.VideoURL).Info("requesting video size")

		res, err := http.Head(videoInfo.VideoURL)
		if err != nil {
			err := fmt.Errorf("error getting size of video %w", err)
			log.Error(err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		contentLength := utils.SafeParseInt64(res.Header.Get("Content-Length"), -1)
		if contentLength == -1 {
			err := fmt.Errorf("error calculating size of video")
			log.WithField("contentLength", contentLength).Error(err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if contentLength > a.Config.Server.MaxUploadSize {
			err := fmt.Errorf(
				"imported video would exceed maximum upload size of %s",
				humanize.Bytes(uint64(a.Config.Server.MaxUploadSize)),
			)
			log.
				WithField("contentLength", contentLength).
				WithField("max_upload_size", a.Config.Server.MaxUploadSize).
				Error(err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		log.WithField("contentLength", contentLength).Info("downloading video")

		if err := utils.Download(videoInfo.VideoURL, uf.Name()); err != nil {
			err := fmt.Errorf("error downloading video %s: %w", url, err)
			log.Error(err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		tf, err := ioutil.TempFile(
			a.Config.Server.UploadPath,
			fmt.Sprintf("tube-transcode-*.mp4"),
		)
		if err != nil {
			err := fmt.Errorf("error creating temporary file for transcoding: %w", err)
			log.Error(err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		vf := filepath.Join(
			a.Library.Paths[collection].Path,
			fmt.Sprintf("%s.mp4", shortuuid.New()),
		)
		thumbFn1 := fmt.Sprintf("%s.jpg", strings.TrimSuffix(tf.Name(), filepath.Ext(tf.Name())))
		thumbFn2 := fmt.Sprintf("%s.jpg", strings.TrimSuffix(vf, filepath.Ext(vf)))

		if err := utils.Download(videoInfo.ThumbnailURL, thumbFn1); err != nil {
			err := fmt.Errorf("error downloading thumbnail: %w", err)
			log.Error(err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		// TODO: Use a proper Job Queue and make this async
		if err := utils.RunCmd(
			a.Config.Transcoder.Timeout,
			"ffmpeg",
			"-y",
			"-i", uf.Name(),
			"-vcodec", "h264",
			"-acodec", "aac",
			"-strict", "-2",
			"-loglevel", "quiet",
			"-metadata", fmt.Sprintf("title=%s", videoInfo.Title),
			"-metadata", fmt.Sprintf("comment=%s", videoInfo.Description),
			tf.Name(),
		); err != nil {
			err := fmt.Errorf("error transcoding video: %w", err)
			log.Error(err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		if err := os.Rename(thumbFn1, thumbFn2); err != nil {
			err := fmt.Errorf("error renaming generated thumbnail: %w", err)
			log.Error(err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		if err := os.Rename(tf.Name(), vf); err != nil {
			err := fmt.Errorf("error renaming transcoded video: %w", err)
			log.Error(err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		// TODO: Make this a background job
		// Resize for lower quality options
		for size, suffix := range a.Config.Transcoder.Sizes {
			log.
				WithField("size", size).
				WithField("vf", filepath.Base(vf)).
				Info("resizing video for lower quality playback")
			sf := fmt.Sprintf(
				"%s#%s.mp4",
				strings.TrimSuffix(vf, filepath.Ext(vf)),
				suffix,
			)

			if err := utils.RunCmd(
				a.Config.Transcoder.Timeout,
				"ffmpeg",
				"-y",
				"-i", vf,
				"-s", size,
				"-c:v", "libx264",
				"-c:a", "aac",
				"-crf", "18",
				"-strict", "-2",
				"-loglevel", "quiet",
				"-metadata", fmt.Sprintf("title=%s", videoInfo.Title),
				"-metadata", fmt.Sprintf("comment=%s", videoInfo.Description),
				sf,
			); err != nil {
				err := fmt.Errorf("error transcoding video: %w", err)
				log.Error(err)
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
		}

		fmt.Fprintf(w, "Video successfully imported!")
	} else {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
	}
}

// HTTP handler for /v/id
func (a *App) pageHandler(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	id := vars["id"]
	prefix, ok := vars["prefix"]
	if ok {
		id = path.Join(prefix, id)
	}
	log.Printf("/v/%s", id)
	playing, ok := a.Library.Videos[id]
	if !ok {
		sort := strings.ToLower(r.URL.Query().Get("sort"))
		quality := strings.ToLower(r.URL.Query().Get("quality"))
		ctx := &struct {
			Sort     string
			Quality  string
			Config   *Config
			Playing  *media.Video
			Playlist media.Playlist
		}{
			Sort:     sort,
			Quality:  quality,
			Config:   a.Config,
			Playing:  &media.Video{ID: ""},
			Playlist: a.Library.Playlist(),
		}
		a.render("upload", w, ctx)
		return
	}

	views, err := a.Store.GetViews(id)
	if err != nil {
		err := fmt.Errorf("error retrieving views for %s: %w", id, err)
		log.Warn(err)
	}

	playing.Views = views

	playlist := a.Library.Playlist()

	// TODO: Optimize this? Bitcask has no concept of MultiGet / MGET
	for _, video := range playlist {
		views, err := a.Store.GetViews(video.ID)
		if err != nil {
			err := fmt.Errorf("error retrieving views for %s: %w", video.ID, err)
			log.Warn(err)
		}
		video.Views = views
	}

	sort := strings.ToLower(r.URL.Query().Get("sort"))
	switch sort {
	case "views":
		media.By(media.SortByViews).Sort(playlist)
	case "", "timestamp":
		media.By(media.SortByTimestamp).Sort(playlist)
	default:
		// By default the playlist is sorted by Timestamp
		log.Warnf("invalid sort critiera: %s", sort)
	}

	quality := strings.ToLower(r.URL.Query().Get("quality"))
	switch quality {
	case "", "720p", "480p", "360p", "240p":
	default:
		log.WithField("quality", quality).Warn("invalid quality")
		quality = ""
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	ctx := &struct {
		Sort     string
		Quality  string
		Config   *Config
		Playing  *media.Video
		Playlist media.Playlist
	}{
		Sort:     sort,
		Quality:  quality,
		Config:   a.Config,
		Playing:  playing,
		Playlist: playlist,
	}
	a.render("index", w, ctx)
}

// HTTP handler for /v/id.mp4
func (a *App) videoHandler(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	id := vars["id"]

	prefix, ok := vars["prefix"]
	if ok {
		id = path.Join(prefix, id)
	}

	log.Printf("/v/%s", id)

	m, ok := a.Library.Videos[id]
	if !ok {
		return
	}

	var videoPath string

	quality := strings.ToLower(r.URL.Query().Get("quality"))
	switch quality {
	case "720p", "480p", "360p", "240p":
		videoPath = fmt.Sprintf(
			"%s#%s.mp4",
			strings.TrimSuffix(m.Path, filepath.Ext(m.Path)),
			quality,
		)
		if !utils.FileExists(videoPath) {
			log.
				WithField("quality", quality).
				WithField("videoPath", videoPath).
				Warn("video with specified quality does not exist (defaulting to default quality)")
			videoPath = m.Path
		}
	case "":
		videoPath = m.Path
	default:
		log.WithField("quality", quality).Warn("invalid quality")
		videoPath = m.Path
	}

	if err := a.Store.Migrate(prefix, id); err != nil {
		err := fmt.Errorf("error migrating store data: %w", err)
		log.Warn(err)
	}

	if err := a.Store.IncViews(id); err != nil {
		err := fmt.Errorf("error updating view for %s: %w", id, err)
		log.Warn(err)
	}

	title := m.Title
	disposition := "attachment; filename=\"" + title + ".mp4\""
	w.Header().Set("Content-Disposition", disposition)
	w.Header().Set("Content-Type", "video/mp4")
	http.ServeFile(w, r, videoPath)
}

// HTTP handler for /t/id
func (a *App) thumbHandler(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	id := vars["id"]
	prefix, ok := vars["prefix"]
	if ok {
		id = path.Join(prefix, id)
	}
	log.Printf("/t/%s", id)
	m, ok := a.Library.Videos[id]
	if !ok {
		return
	}
	w.Header().Set("Cache-Control", "public, max-age=7776000")
	if m.ThumbType == "" {
		w.Header().Set("Content-Type", "image/jpeg")
		w.Write(static.MustGetFile("defaulticon.jpg"))
	} else {
		w.Header().Set("Content-Type", m.ThumbType)
		w.Write(m.Thumb)
	}
}

// HTTP handler for /feed.xml
func (a *App) rssHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "public, max-age=7776000")
	w.Header().Set("Content-Type", "text/xml")
	w.Write(a.Feed)
}
