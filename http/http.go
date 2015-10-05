package http

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/danderson/pixiecore/api"
	"github.com/danderson/pixiecore/log"
)

// pxelinux configuration that tells the PXE/UNDI stack to boot from
// local disk.
const bootFromDisk = `
DEFAULT local
LABEL local
LOCALBOOT 0
`

// A silly limerick displayed while pxelinux loads big OS
// images. Possibly the most important piece of this program.
const limerick = `
	        There once was a protocol called PXE,
	        Whose specification was overly tricksy.
	        A committee refined it,
	        Into a big Turing tarpit,
	        And now you're using it to boot your PC.
`

type httpServer struct {
	booter  api.Booter
	ldlinux []byte
	key     [32]byte // to sign URLs
}

func (s *httpServer) Ldlinux(w http.ResponseWriter, r *http.Request) {
	log.Debug("HTTP", "Starting send of ldlinux.c32 to %s (%d bytes)", r.RemoteAddr, len(s.ldlinux))
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Write(s.ldlinux)
	log.Log("HTTP", "Sent ldlinux.c32 to %s (%d bytes)", r.RemoteAddr, len(s.ldlinux))
}

func (s *httpServer) PxelinuxConfig(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain")

	macStr := filepath.Base(r.URL.Path)
	errStr := fmt.Sprintf("%s requested a pxelinux config from URL %q, which does not include a MAC address", r.RemoteAddr, r.URL)
	if !strings.HasPrefix(macStr, "01-") {
		log.Debug("HTTP", errStr)
		http.Error(w, "Missing MAC address in request", http.StatusBadRequest)
		return
	}
	mac, err := net.ParseMAC(macStr[3:])
	if err != nil {
		log.Debug("HTTP", errStr)
		http.Error(w, "Malformed MAC address in request", http.StatusBadRequest)
		return
	}

	spec, err := s.booter.BootSpec(mac)
	if err != nil {
		// We have a machine sitting in pxelinux, but the Booter says
		// we shouldn't be netbooting. So, give it a config that tells
		// pxelinux to shut down PXE booting and continue with the
		// next local boot method.
		log.Debug("HTTP", "Telling pxelinux on %s (%s) to boot from disk because of API server verdict: %s", mac, r.RemoteAddr, err)
		w.Write([]byte(bootFromDisk))
		return
	}

	// The file IDs can be arbitrary blobs that make sense to the
	// Booter, but pxelinux speaks URL, so we need to encode the
	// blobs.
	spec.Kernel = "f/" + base64.URLEncoding.EncodeToString([]byte(spec.Kernel))
	for i := range spec.Initrd {
		spec.Initrd[i] = "f/" + base64.URLEncoding.EncodeToString([]byte(spec.Initrd[i]))
	}

	cfg := fmt.Sprintf(`
SAY %s
DEFAULT linux
LABEL linux
LINUX %s
APPEND initrd=%s %s
`, strings.Replace(limerick, "\n", "\nSAY ", -1), spec.Kernel, strings.Join(spec.Initrd, ","), spec.Cmdline)

	w.Write([]byte(cfg))
	log.Log("HTTP", "Sent pxelinux config to %s (%s)", mac, r.RemoteAddr)
}

func (s *httpServer) File(w http.ResponseWriter, r *http.Request) {
	encodedID := filepath.Base(r.URL.Path)
	id, err := base64.URLEncoding.DecodeString(encodedID)
	if err != nil {
		log.Log("http", "Bad base64 encoding for URL %q from %s: %s", r.URL, r.RemoteAddr, err)
		http.Error(w, "Malformed file ID", http.StatusBadRequest)
		return
	}
	f, pretty, err := s.booter.File(string(id))
	if err != nil {
		log.Log("HTTP", "Couldn't get byte stream for %q from %s: %s", r.URL, r.RemoteAddr, err)
		http.Error(w, "Couldn't get byte stream", http.StatusInternalServerError)
		return
	}
	defer f.Close()

	w.Header().Set("Content-Type", "application/octet-stream")
	written, err := io.Copy(w, f)
	if err != nil {
		log.Log("HTTP", "Error serving %s to %s: %s", pretty, r.RemoteAddr, err)
		return
	}
	log.Log("HTTP", "Sent %s to %s (%d bytes)", pretty, r.RemoteAddr, written)
}

func ServeHTTP(port int, booter api.Booter, ldlinux []byte) error {
	s := &httpServer{
		booter:  booter,
		ldlinux: ldlinux,
	}
	if _, err := io.ReadFull(rand.Reader, s.key[:]); err != nil {
		return fmt.Errorf("cannot initialize ephemeral signing key: %s", err)
	}

	http.HandleFunc("/ldlinux.c32", s.Ldlinux)
	http.HandleFunc("/pxelinux.cfg/", s.PxelinuxConfig)
	http.HandleFunc("/f/", s.File)

	log.Log("HTTP", "Listening on port %d", port)
	return http.ListenAndServe(fmt.Sprintf(":%d", port), nil)
}
