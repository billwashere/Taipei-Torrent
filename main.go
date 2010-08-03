package main

import (
	"flag"
	"log"
	"os"
	"taipei"
)

var torrent string
var debugp bool

func init() {
	flag.StringVar(&torrent, "torrent", "", "URL or path to a torrent file (Required)")
	flag.BoolVar(&debugp, "debug", false, "Turn on debugging")
}

func checkRequiredFlags() {
	req := []string{"torrent"}
	for _, n := range req {
		f := flag.Lookup(n)
		if f.DefValue == f.Value.String() {
			log.Stderrf("Required flag not set: -%s", f.Name)
			flag.Usage()
			os.Exit(1)
		}
	}
}

func main() {
	flag.Parse()
	checkRequiredFlags()
	log.Stderr("Starting.")
	// Auxiliary web server. Currently only displays session stats.
	syncStatus := taipei.WebServer()
	// Bittorrent.
	ts, err := taipei.NewTorrentSession(torrent, syncStatus)
	if err != nil {
		log.Stderr("Could not create torrent session.", err)
		return
	}
	err = ts.DoTorrent()
	if err != nil {
		log.Stderr("Failed: ", err)
	} else {
		log.Stderr("Done")
	}
}
