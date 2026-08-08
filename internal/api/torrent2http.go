package api

// torrent2http compatibility layer.
//
// torrent2http (steeve/torrent2http, used by older Kodi addons like Quasar) is a
// single-torrent-per-process bridge: its HTTP API (/status, /ls, /files/<path>)
// carries no torrent identifier because each process serves exactly one magnet.
//
// FluxTorrent is a persistent multi-torrent service, so a perfectly transparent
// drop-in isn't possible — there's no hash in the original requests. This layer
// therefore operates on a *selected* torrent: the `?hash=` query if provided,
// otherwise the most recently added active torrent. That covers the common
// "one stream at a time" usage; for multiple concurrent torrents prefer the
// native API or the TorrServer layer.
//
// Implemented:
//   GET /status            → session/torrent status JSON
//   GET /ls                → file list JSON
//   GET /files/{path...}   → stream a file by its path (Range-capable)
//   GET /get/{index}       → stream a file by 0-based index

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/jodacame/fluxtorrent/internal/engine"
)

func (s *Server) registerTorrent2HTTP(mux *http.ServeMux) {
	mux.HandleFunc("GET /status", s.t2hStatus)
	mux.HandleFunc("GET /ls", s.t2hLs)
	mux.HandleFunc("GET /files/{path...}", s.t2hFiles)
	mux.HandleFunc("GET /get/{index}", s.t2hGet)
}

// currentTorrent resolves the torrent a torrent2http request targets, returning
// the snapshot itself rather than a hash to look up again: a torrent can be
// dropped between the two calls (drop-after-playback fires the moment the last
// reader closes), and a second lookup would then hand the handlers a nil Info.
func (s *Server) currentTorrent(r *http.Request) (*engine.Info, bool) {
	if h := r.URL.Query().Get("hash"); h != "" {
		if info, ok := s.eng.Get(h); ok {
			return info, true
		}
	}
	list := s.eng.List() // sorted newest-first
	if len(list) == 0 {
		return nil, false
	}
	return &list[0], true
}

// GET /status — session/torrent status.
func (s *Server) t2hStatus(w http.ResponseWriter, r *http.Request) {
	info, ok := s.currentTorrent(r)
	if !ok {
		writeJSON(w, http.StatusOK, map[string]any{"state": -1, "state_str": "Idle"})
		return
	}
	state, stateStr := t2hState(info.Stats.State)
	writeJSON(w, http.StatusOK, map[string]any{
		"name":          info.Name,
		"state":         state,
		"state_str":     stateStr,
		"progress":      info.Stats.Progress * 100,
		"download_rate": info.Stats.DownKbps * 1000 / 8 / 1024, // kB/s
		"upload_rate":   info.Stats.UpKbps * 1000 / 8 / 1024,
		"num_peers":     info.Stats.Peers,
		"num_seeds":     info.Stats.Seeders,
		"total_size":    info.SizeB,
	})
}

// GET /ls — file list.
func (s *Server) t2hLs(w http.ResponseWriter, r *http.Request) {
	info, ok := s.currentTorrent(r)
	if !ok {
		writeJSON(w, http.StatusOK, map[string]any{"files": []any{}})
		return
	}
	files := make([]map[string]any, 0, len(info.Files))
	var offset int64
	for _, f := range info.Files {
		files = append(files, map[string]any{
			"name":     f.Path,
			"size":     f.SizeB,
			"offset":   offset,
			"download": int64(info.Stats.Progress * float64(f.SizeB)),
			"progress": info.Stats.Progress * 100,
			// stable stream URL by index for clients that build their own links
			"url": "/get/" + strconv.Itoa(f.Index) + "?hash=" + info.Hash,
		})
		offset += f.SizeB
	}
	writeJSON(w, http.StatusOK, map[string]any{"files": files})
}

// GET /files/{path...} — stream a file by its path.
func (s *Server) t2hFiles(w http.ResponseWriter, r *http.Request) {
	info, ok := s.currentTorrent(r)
	if !ok {
		writeErr(w, http.StatusNotFound, "no active torrent")
		return
	}
	want := strings.TrimPrefix(r.PathValue("path"), "/")
	for _, f := range info.Files {
		if f.Path == want || strings.HasSuffix(f.Path, want) {
			s.serveStream(w, r, info.Hash, f.Index)
			return
		}
	}
	writeErr(w, http.StatusNotFound, "file not found in torrent")
}

// GET /get/{index} — stream a file by 0-based index.
func (s *Server) t2hGet(w http.ResponseWriter, r *http.Request) {
	info, ok := s.currentTorrent(r)
	if !ok {
		writeErr(w, http.StatusNotFound, "no active torrent")
		return
	}
	idx, err := strconv.Atoi(r.PathValue("index"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "index must be an integer")
		return
	}
	s.serveStream(w, r, info.Hash, idx)
}

// t2hState maps an engine state to torrent2http's numeric state + string.
// torrent2http enum: 2 metadata, 3 downloading, 4 finished, 5 seeding.
func t2hState(state string) (int, string) {
	switch state {
	case "fetching":
		return 2, "Downloading metadata"
	case "searching":
		return 3, "Finding peers"
	case "ready":
		return 4, "Finished"
	case "seeding":
		return 5, "Seeding"
	default: // downloading / playing
		return 3, "Downloading"
	}
}
