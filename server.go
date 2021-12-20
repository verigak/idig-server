package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"path/filepath"
	"sort"
	"strings"
)

type Server struct {
	Root string
}

type PushRequest struct {
	UID      string            `json:"uid"`
	UserName string            `json:"username"`
	Message  string            `json:"message"`
	Head     string            `json:"head"`
	Surveys  map[string]Survey `json:"surveys"`
}

type PushResponse struct {
	Status  string   `json:"status"`
	Version string   `json:"version"`
	Missing []string `json:"missing,omitempty"`
	Updates []Patch  `json:"updates,omitempty"`
}

const (
	StatusOK       = "ok"
	StatusPushed   = "pushed"
	StatusConflict = "conflict"
	StatusMissing  = "missing"
)

type Patch struct {
	Id  string `json:"id"`
	Old Survey `json:"old"`
	New Survey `json:"new"`
}

type Survey map[string]string

func (s Survey) IsEqual(t Survey) bool {
	keys := s.Keys()
	keys.FormUnion(t.Keys())
	for key := range keys {
		if s[key] != t[key] {
			return false
		}
	}
	return true
}

func (s Survey) Keys() Set {
	keys := make(Set, len(s))
	for key := range s {
		keys[key] = struct{}{}
	}
	return keys
}

type Version map[string]Survey

func (v Version) Keys() Set {
	keys := make(Set, len(v))
	for key := range v {
		keys[key] = struct{}{}
	}
	return keys
}

type Set map[string]struct{}

func (s Set) FormUnion(a Set) {
	for k := range a {
		s[k] = struct{}{}
	}
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	log.Printf("> %s %s", r.Method, r.URL)
	code, err := s.serve(w, r)
	if err != nil {
		log.Printf("< error %s", err)
		http.Error(w, err.Error(), code)
	} else {
		if code != http.StatusOK {
			w.WriteHeader(code)
		}
	}
}

func (s Server) serve(w http.ResponseWriter, r *http.Request) (int, error) {
	t := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(t) == 0 {
		return http.StatusBadRequest, fmt.Errorf("Missing trench")
	}
	for _, s := range t[1:] {
		if s == ".." {
			return http.StatusBadRequest, fmt.Errorf("Invalid path")
		}
	}
	trench := t[0]
	name := strings.Join(t[1:], "/")

	dir := filepath.Join(s.Root, trench)
	repo, err := OpenRepository(dir)
	if err != nil {
		return http.StatusNotFound, fmt.Errorf("Failed to open trench '%s': %w", trench, err)
	}
	defer repo.Close()

	switch r.Method {
	case http.MethodGet:
		return s.handleReadAttachment(w, r, repo, name)
	case http.MethodPut:
		return s.handleWriteAttachment(w, r, repo, name)
	case http.MethodPost:
		return s.handlePush(w, r, repo)
	default:
		return http.StatusMethodNotAllowed, fmt.Errorf("%s not allowed", r.Method)
	}
}

func (s *Server) handleReadAttachment(w http.ResponseWriter, r *http.Request, repo *Repository, name string) (int, error) {
	checksum := strings.Trim(r.Header.Get("If-Match"), "\"")
	log.Printf("> read %s [%s]", name, checksum)
	f, err := repo.OpenAttachment(name, checksum)
	if err != nil {
		return http.StatusNotFound, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return http.StatusInternalServerError, err
	}
	w.Header().Set("ETag", fmt.Sprintf("\"%s\"", checksum))
	http.ServeContent(w, r, name, fi.ModTime(), f)
	log.Printf("< read %s [%s] (%d bytes)", name, checksum, fi.Size())
	return http.StatusOK, nil
}

func (s *Server) handleWriteAttachment(w http.ResponseWriter, r *http.Request, repo *Repository, name string) (int, error) {
	defer func() {
		// Drain the body and close
		io.Copy(io.Discard, r.Body)
		r.Body.Close()
	}()
	if name == "" {
		return http.StatusBadRequest, fmt.Errorf("Invalid attachment name")
	}
	checksum := strings.Trim(r.Header.Get("ETag"), "\"")
	if checksum == "" {
		return http.StatusBadRequest, fmt.Errorf("Missing etag")
	}
	log.Printf("> write %s [%s]", name, checksum)
	err := repo.WriteAttachment(name, checksum, r.Body)
	if err != nil {
		return http.StatusInternalServerError, fmt.Errorf("Could not write attachment '%s': %w", name, err)
	} else {
		log.Printf("< wrote %s [%s]", name, checksum)
		return http.StatusOK, nil
	}
}

func (s *Server) handlePush(w http.ResponseWriter, r *http.Request, repo *Repository) (int, error) {
	var req PushRequest
	dec := json.NewDecoder(r.Body)
	err := dec.Decode(&req)
	if err != nil {
		return http.StatusBadRequest, fmt.Errorf("Invalid sync request: %w", err)
	}

	log.Printf("> push %s {uid: %q, username: %q, surveys: <%d surveys>}",
		req.Head, req.UID, req.UserName, len(req.Surveys))

	head := repo.Head()

	if head != "" {
		if req.Head == "" || req.Head != head {
			// We are not on the same version, client should pull
			old := make(Version)
			new, err := repo.ReadSurveys()
			if err != nil {
				return http.StatusInternalServerError, err
			}

			if req.Head != "" {
				// Try to checkout this version.
				// If we fail, fallback to the empty version
				err := repo.Checkout(req.Head)
				if err == nil {
					old, err = repo.ReadSurveys()
					if err != nil {
						return http.StatusInternalServerError, err
					}
				}
			}

			resp := PushResponse{
				Status:  StatusConflict,
				Version: head,
				Updates: diffVersions(old, new),
			}
			log.Printf("< conflict %s [<%d updates>]", resp.Version, len(resp.Updates))
			return s.writeJSON(w, r, &resp)
		}
	}

	missing := s.missingAttachments(repo, req.Surveys)
	if len(missing) > 0 {
		// Missing attachments
		resp := PushResponse{
			Status:  StatusMissing,
			Version: head,
			Missing: missing,
		}
		if len(missing) < 4 {
			log.Printf("< missing [%s]", strings.Join(missing, ", "))
		} else {
			log.Printf("< missing [<%d attachments>]", len(missing))
		}
		_, err := s.writeJSON(w, r, &resp)
		return http.StatusOK, err
	}

	newHead, err := repo.Commit(req.UID, req.UserName, req.Message, req.Surveys)
	if err != nil {
		return http.StatusInternalServerError, err
	}

	if newHead != head {
		resp := PushResponse{
			Status:  StatusPushed,
			Version: newHead,
		}
		log.Printf("< pushed %s", newHead)
		return s.writeJSON(w, r, &resp)
	} else {
		resp := PushResponse{
			Status:  StatusOK,
			Version: head,
		}
		log.Printf("< ok %s", head)
		return s.writeJSON(w, r, &resp)
	}
}

func diffVersions(old, new Version) []Patch {
	var patches []Patch
	ids := old.Keys()
	ids.FormUnion(new.Keys())
	for id := range ids {
		o := old[id]
		n := new[id]
		if !o.IsEqual(n) {
			patches = append(patches, Patch{Id: id, Old: o, New: n})
		}
	}
	return patches
}

func (s *Server) writeJSON(w http.ResponseWriter, r *http.Request, v interface{}) (int, error) {
	w.Header().Set("Content-Type", "application/json")

	enc := json.NewEncoder(w)
	if r.URL.Query().Has("debug") {
		enc.SetIndent("", "  ")
	}
	err := enc.Encode(v)
	if err != nil {
		return http.StatusInternalServerError, err
	} else {
		return http.StatusOK, nil
	}
}

func (s *Server) missingAttachments(repo *Repository, surveys map[string]Survey) []string {
	var missing []string
	for _, survey := range surveys {
		attachments := strings.Split(survey["RelationAttachments"], "\n\n")
		for _, a := range attachments {
			var name, ts string
			for _, s := range strings.Split(a, "\n") {
				key, val := Cut(s, "=")
				if key == "n" {
					name = val
				} else if key == "d" {
					ts = val
				}
			}
			if name != "" && ts != "" && !repo.ExistsAttachment(name, ts) {
				missing = append(missing, name)
			}
		}
	}
	sort.Strings(missing)
	return missing
}

func Cut(s, sep string) (before, after string) {
	if i := strings.Index(s, sep); i >= 0 {
		return s[:i], s[i+len(sep):]
	}
	return s, ""
}
