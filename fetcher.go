package main

import (
	"crypto/md5"
	"flag"
	"fmt"
	"github.com/iand/feedparser"
	"github.com/iand/imgpick"
	"github.com/iand/salience"
	"github.com/placetime/datastore"
	"image/png"
	"io"
	"log"
	"net/http"
	"os"
	"path"
	"runtime"
	"time"
)

var (
	imgDir       = "/var/opt/timescroll/img"
	feedInterval = 30
	runOnce      = false
	feedurl      = ""
)

func main() {

	runtime.GOMAXPROCS(runtime.NumCPU())

	flag.StringVar(&imgDir, "images", "/var/opt/timescroll/img", "filesystem directory to store fetched images")
	flag.IntVar(&feedInterval, "feedinterval", 30, "interval for checking feeds (minutes)")
	flag.BoolVar(&runOnce, "runonce", false, "run the fetcher once and then exit")
	flag.StringVar(&feedurl, "debugfeed", "", "run the fetcher on the given feed url and debug results")
	flag.Parse()

	if feedurl != "" {
		debugFeed(feedurl)
		return
	}

	checkEnvironment()
	log.Printf("Image directory: %s", imgDir)

	pollFeeds()
	pollImages()

	if runOnce {
		return
	}

	ticker := time.Tick(30 * time.Minute)
	for _ = range ticker {
		pollFeeds()
		pollImages()
	}

}

func checkEnvironment() {
	f, err := os.Open(imgDir)
	if err != nil {
		log.Printf("Could not open image path %s: %s", imgDir, err.Error())
		os.Exit(1)
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		log.Printf("Could not stat image path %s: %s", imgDir, err.Error())
		os.Exit(1)
	}

	if !fi.IsDir() {
		log.Printf("Image path is not a directory %s: %s", imgDir, err.Error())
		os.Exit(1)
	}

}

func pollFeeds() {
	log.Print("Refreshing feeds")
	s := datastore.NewRedisStore()
	defer s.Close()

	profiles, _ := s.FeedDrivenProfiles()

	jobs := make(chan *datastore.Profile, len(profiles))
	results := make(chan *ProfileItemData, len(profiles))

	for w := 0; w < 3; w++ {
		go feedWorker(w, jobs, results)
	}

	for _, p := range profiles {
		jobs <- p
	}
	close(jobs)

	for i := 0; i < len(profiles); i++ {
		data := <-results
		if data.Error != nil {
			log.Printf("Error processing feed for %s: %v", data.Profile.Pid, data.Error)
		} else {
			log.Printf("Found %d items in feed for %s", len(data.Items), data.Profile.Pid)
		}

		updateProfileItemData(data)
		runtime.Gosched()
	}

}

func pollImages() {
	log.Print("Fetching images")
	s := datastore.NewRedisStore()
	defer s.Close()

	items, _ := s.GrabItemsNeedingImages(30)
	log.Printf("%d images need to be fetched", len(items))
	if len(items) > 0 {
		jobs := make(chan *datastore.Item, len(items))
		results := make(chan *ItemImageData, len(items))

		for w := 0; w < 3; w++ {
			go imageWorker(w, jobs, results)
		}

		for _, p := range items {
			jobs <- p
		}
		close(jobs)

		for i := 0; i < len(items); i++ {
			data := <-results
			if data.Error != nil {
				log.Printf("Error processing images for %s: %v", data.Item.Id, data.Error)
			} else {
				log.Printf("Found image %s for %s", data.Item.Image, data.Item.Id)
			}

			s.UpdateItem(data.Item)
			runtime.Gosched()
		}
	}
}

type ProfileItemData struct {
	Profile *datastore.Profile
	Items   []*datastore.Item
	Error   error
}

type ItemImageData struct {
	Item  *datastore.Item
	Error error
}

func feedWorker(id int, jobs <-chan *datastore.Profile, results chan<- *ProfileItemData) {
	for p := range jobs {
		log.Printf("Feed worker %d processing feed %s", id, p.FeedUrl)

		resp, err := http.Get(p.FeedUrl)

		if err != nil {
			log.Printf("Feed worker %d got http error  %s", id, err.Error())
			results <- &ProfileItemData{p, nil, err}
			continue
		}
		defer resp.Body.Close()

		feed, err := feedparser.NewFeed(resp.Body)

		results <- &ProfileItemData{p, itemsFromFeed(p.Pid, feed), err}
	}
}

func itemsFromFeed(pid string, feed *feedparser.Feed) []*datastore.Item {

	items := make([]*datastore.Item, 0)
	if feed != nil {
		for _, item := range feed.Items {
			hasher := md5.New()
			io.WriteString(hasher, item.Id)
			id := fmt.Sprintf("%x", hasher.Sum(nil))
			items = append(items, &datastore.Item{Id: id, Pid: pid, Event: item.When.Unix(), Text: item.Title, Link: item.Link, Image: item.Image})
		}
	}
	return items
}

func imageWorker(id int, jobs <-chan *datastore.Item, results chan<- *ItemImageData) {

	for item := range jobs {
		log.Printf("Image worker %d processing item %s", id, item.Id)
		img, err := imgpick.PickImage(item.Link)

		if img == nil || err != nil {
			results <- &ItemImageData{item, err}
			continue
		}

		imgOut := salience.Crop(img, 460, 160)

		filename := fmt.Sprintf("%s.png", item.Id)

		foutName := path.Join(imgDir, filename)

		fout, err := os.OpenFile(foutName, os.O_CREATE|os.O_WRONLY, 0666)
		if err != nil {
			results <- &ItemImageData{item, err}
			continue
		}

		if err = png.Encode(fout, imgOut); err != nil {
			results <- &ItemImageData{item, err}
			continue
		}

		item.Image = filename

		results <- &ItemImageData{item, err}

	}
}

func updateProfileItemData(data *ProfileItemData) error {
	if data.Items != nil {
		s := datastore.NewRedisStore()
		defer s.Close()

		p := data.Profile

		followers, err := s.Followers(p.Pid, p.FollowerCount, 0)
		if err != nil {
			return err
		}

		for _, f := range followers {
			s.Unfollow(f.Pid, p.Pid)
		}

		//s.DeleteMaybeItems(p.Pid)
		for _, item := range data.Items {
			s.AddItem(item.Pid, time.Unix(item.Event, 0), item.Text, item.Link, item.Image, item.Id)
		}

		for _, f := range followers {
			s.Follow(f.Pid, p.Pid)
		}

	}

	return nil
}

func debugFeed(url string) {
	log.Printf("Debugging feed %s", url)
	resp, err := http.Get(url)
	log.Printf("Response: %s", resp.Status)

	if err != nil {
		log.Printf("Fetch of feed got http error  %s", err.Error())
		return
	}

	defer resp.Body.Close()

	feed, err := feedparser.NewFeed(resp.Body)

	for _, item := range feed.Items {
		fmt.Printf("--Item (%s)\n", item.Id)
		fmt.Printf("  Title: %s\n", item.Title)
		fmt.Printf("  Link:  %s\n", item.Link)
		fmt.Printf("  Image: %s\n", item.Image)

		//		s.AddItem(item.Pid, time.Unix(item.Event, 0), item.Text, item.Link, item.Image, item.Id)
	}

}
