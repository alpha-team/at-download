package rssbot

import (
	"bytes"
	"fmt"
	"image"
	"image/gif"
	_ "image/png"
	"io"
	"io/ioutil"
	"math"
	"mime"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	rss "github.com/jteeuwen/go-pkg-rss" // Subscribe to RSS
	"github.com/jteeuwen/go-pkg-xmlx"    // Dependency of RSS
	"github.com/rockneurotiko/go-tgbot"  // Telegram Bot Library :)
	"gopkg.in/fatih/set.v0"              // Set data structure
)

const (
	MAX_RETRIES = 3
	MINS_SLEEP  = 5
)

// BuildBot ...
func BuildBot(dbd string, token string, notify bool) *tgbot.TgBot {
	autodb = dbSync{
		&sync.Mutex{},
		dbd,
	}

	bot := tgbot.New(token).
		MultiCommandFn([]string{`sub +(https?://.+)`, `sub ?.*`}, subs).
		MultiCommandFn([]string{`delete +(\d+)\)? *`, `rm +(\d+)\)? *`}, remove).
		SimpleCommandFn(`list`, list).
		SimpleCommandFn(`help`, help).
		SimpleCommandFn(`start`, help).
		SimpleCommandFn(`cancel`, returnErrorMsg).
		SimpleCommandFn(`preference ?.*`, preferenceFail)

	bot.StartChain().
		CommandFn(`preference (image)`, changePreference).
		SimpleCommandFn(`(enable|disable)`, valuePreference).
		CancelChainCommand(`cancel`, cancelPreference).
		EndChain()

	bot.DefaultDisableWebpagePreview(true)

	removeUnused()

	// Start all saved RSS
	readAllDbRss(*bot)

	sendWakingUpMessage(*bot, notify)

	return bot
}

func help(bot tgbot.TgBot, msg tgbot.Message, text string) *string {
	fmt.Printf("%d asked for help\n", msg.Chat.ID)
	bot.Answer(msg).Text(buildHelp()).ReplyToMessage(msg.ID).End()
	return nil
}

func remove(bot tgbot.TgBot, msg tgbot.Message, args []string, kargs map[string]string) *string {
	id := fmt.Sprintf("%d", msg.Chat.ID)
	key := buildKey("user", id, "") + ":"
	n := args[1]
	i, e := strconv.Atoi(n)
	if e != nil {
		return nil
	}

	fmt.Printf("%d asked to remove %d\n", msg.Chat.ID, i)

	k, err := getNthDb(key, i)
	if err != nil {
		bot.Answer(msg).Text(fmt.Sprintf("Some error happened:\nError: %s", err.Error())).ReplyToMessage(msg.ID).End()
		return nil
	}
	url := strings.TrimLeft(k, key)
	urlkey := buildKey("rss", url, id)

	removeFromCacheUsers(url, msg.Chat.ID)
	multiDeleteDb([]string{k, urlkey})
	bot.Answer(msg).Text(fmt.Sprintf("RSS %s removed!", url)).ReplyToMessage(msg.ID).End()
	return nil
}

func subs(bot tgbot.TgBot, msg tgbot.Message, args []string, kargs map[string]string) *string {
	if len(args) != 2 {
		bot.Answer(msg).Text("Usage of sub command:\n/sub <RSS_url>").ReplyToMessage(msg.ID).End()
		return nil
	}
	url := strings.TrimSpace(args[1])
	fmt.Printf("%d asked to subscribe to %s\n", msg.Chat.ID, url)
	go botPollSubscribe(bot, msg, url, 5, charsetReader)
	return nil
}

func list(bot tgbot.TgBot, msg tgbot.Message, text string) *string {
	fmt.Printf("%d asked for his list.\n", msg.Chat.ID)
	id := fmt.Sprintf("%d", msg.Chat.ID)
	userkey := buildKey("user", id, "")
	allusern := loadFromDbPrefix(userkey + ":")
	var buffer bytes.Buffer
	buffer.WriteString(fmt.Sprintf("You are subscribed to %d RSS.\n---------\n", len(allusern)))
	i := 0
	iterurls := getKeysAndSort(allusern)
	for _, urlkey := range iterurls {
		parts := strings.Split(urlkey, ":")
		url := strings.Join(parts[2:], ":")
		buffer.WriteString(fmt.Sprintf("%d) %s\n", i, url))
		i++
	}
	longSend(bot, msg.Chat.ID, buffer.String())
	return nil
}

func getKeysAndSort(dic map[string]string) []string {
	var res []string
	for k := range dic {
		res = append(res, k)
	}
	sort.Strings(res)
	return res
}

func readAllDbRss(bot tgbot.TgBot) {
	allrss := loadFromDbPrefix("rss:")
	// nmax := len(allrss)
	blocks := 60.0 * float64(MINS_SLEEP) // Now every second in 9 mins. Every half second in a minute, 120 blocks
	nseconds := float64(1)               //60 / float64(blocks)
	// module := int(math.Ceil(float64(nmax) / blocks))
	i := 0
	for urlkey := range allrss {
		splitted := strings.Split(urlkey, ":")
		if len(splitted) != 3 {
			continue
		}
		uri := strings.Join(splitted[1:], ":")

		j := i
		i++
		go func(uri string, firsttime bool) {
			n_errors := 0
			feed := rss.New(5, true, chanHandler, botItemHandler(bot, firsttime))

			// Start calcule by groups, calculate how many sleep before execute
			timeofsleep := math.Mod(float64(j), blocks) * nseconds // float64(int(j/module)) * nseconds
			start := time.Now()
			<-time.After(time.Duration(int(timeofsleep*1000)) * time.Millisecond)
			fmt.Printf("%d (%s) started after %v seconds\n", j, uri, time.Since(start))

			for {
				if err := feed.Fetch(uri, charsetReader); err != nil {
					// fmt.Fprintf(os.Stderr, "[e] %s: %s\n", uri, err)
					// if isAffordableNetworkError(err) && n_errors < MAX_RETRIES {
					if n_errors < MAX_RETRIES {
						fmt.Fprintf(os.Stderr, "[e] (%d/%d) %s: %s\n", n_errors, MAX_RETRIES, uri, err)
						n_errors++
						// <-time.After(time.Duration(feed.SecondsTillUpdate() * 1e9))
						<-time.After(time.Duration(MINS_SLEEP) * time.Minute)
						continue
					} else {
						fmt.Fprintf(os.Stderr, "[e] (%d/%d) %s: %s\n", n_errors, MAX_RETRIES, uri, err)
						fmt.Printf("Removing url %s\n", uri)
						cleanBadUrl(uri)
						return
					}
				}
				n_errors = 0

				if firsttime {
					firsttime = false
					setRssValue(urlkey, "true")
				}
				// Every MINS_SLEEP mins
				// <-time.After(time.Duration(feed.SecondsTillUpdate() * 1e9))
				<-time.After(time.Duration(MINS_SLEEP) * time.Minute)
			}
		}(uri, true)
	}
}

func changePreference(bot tgbot.TgBot, msg tgbot.Message, args []string, kargs map[string]string) *string {

	setPreference(msg.Chat.ID, args[1])
	pr := preferencesNameDescr[args[1]]
	kbd := keyboardFromPreference(pr)
	text := textFromOption(pr)

	bot.Answer(msg).Text(text).ReplyToMessage(msg.ID).Keyboard(kbd).End()
	return nil
}

func valuePreference(bot tgbot.TgBot, msg tgbot.Message, text string) *string {
	newv := strings.TrimLeft(text, "/")
	changed := changePreferenceTo(msg.Chat.ID, newv)
	answer := fmt.Sprintf("The value is now %s", newv)
	if !changed {
		answer = "Some error happened, sorry."
	}

	kh := tgbot.ReplyKeyboardHide{true, true}

	bot.Answer(msg).Text(answer).KeyboardHide(kh).End()
	return nil
}

func cancelPreference(bot tgbot.TgBot, msg tgbot.Message, text string) *string {
	deletePreference(msg.Chat.ID)
	kh := tgbot.ReplyKeyboardHide{true, true}
	bot.Answer(msg).Text("Canceled!").KeyboardHide(kh).End()
	return nil
}

func preferenceFail(bot tgbot.TgBot, msg tgbot.Message, text string) *string {
	send := `You tried to execute the preference command, but you didn't executed well, right now, the preferences are:
/preference image`
	bot.Answer(msg).Text(send).ReplyToMessage(msg.ID).End()
	return nil
}

func returnErrorMsg(bot tgbot.TgBot, msg tgbot.Message, text string) *string {
	messages := map[string]string{
		"/cancel": "You are not in a process that can be cancellable.",
	}
	answer := messages[text]
	if answer != "" {
		bot.Answer(msg).Text(answer).ReplyToMessage(msg.ID).End()
	}
	return nil
}

func saveAllValues(uri string, id string) {
	urlkey := buildKey("rss", uri, "")
	urluserkey := buildKey("rss", uri, id)
	userkey := buildKey("user", id, "")
	userurlkey := buildKey("user", id, uri)
	saveInDb(map[string]string{
		urlkey:     "true",
		urluserkey: "true",
		userurlkey: "true",
		userkey:    "true",
	})
}

func checkCache(uri string, id string) bool {
	urlkey := buildKey("rss", uri, "")
	_, ok := getRssValue(urlkey)
	if ok {
		saveAllValues(uri, id)
		return true
	}
	return false
}

func checkDb(uri string, id string) bool {
	urlkey := buildKey("rss", uri, "")
	val := loadFromDb(urlkey)
	if val != "" {
		saveAllValues(uri, id)
		setRssValue(urlkey, "true")
		return true
	}
	return false
}

func removeFromCacheUsers(uri string, id int) {
	removeRssInnerUser(uri, id)
}

func appendToCacheUsers(uri string, id int) {
	addRssInnerUser(uri, id)
}

func botPollSubscribe(bot tgbot.TgBot, msg tgbot.Message, uri string, timeout int, cr xmlx.CharsetFunc) {
	// If already a rss, only add him to the db
	// First, in cache:
	id := fmt.Sprintf("%d", msg.Chat.ID)
	ok := checkCache(uri, id)
	if ok {
		appendToCacheUsers(uri, msg.Chat.ID)
		bot.Answer(msg).Text(fmt.Sprintf("You have been subscribed to %s", uri)).ReplyToMessage(msg.ID).End()
		return
	}
	ok = checkDb(uri, id)
	if ok {
		appendToCacheUsers(uri, msg.Chat.ID)
		bot.Answer(msg).Text(fmt.Sprintf("You have been subscribed to %s", uri)).ReplyToMessage(msg.ID).End()
		return
	}

	firsttime := true
	n_errors := 0
	// Adding new RSS
	feed := rss.New(timeout, true, chanHandler, botItemHandler(bot, firsttime))

	for {
		if err := feed.Fetch(uri, cr); err != nil {
			// fmt.Fprintf(os.Stderr, "[e] %s: %s\n", uri, err)
			if !firsttime && n_errors < MAX_RETRIES {
				fmt.Fprintf(os.Stderr, "[e] (%d/%d) %s: %s\n", n_errors, MAX_RETRIES, uri, err)
				n_errors++
				<-time.After(time.Duration(10) * time.Second)
				continue
			} else {
				if firsttime {
					bot.Answer(msg).Text(fmt.Sprintf("Bad RSS: %s, maybe the URL is bad.\nError msg: %s", uri, err.Error())).ReplyToMessage(msg.ID).End()
				}
				fmt.Printf("Removing url %s\n", uri)
				cleanBadUrl(uri)
				return
			}
		}

		n_errors = 0
		if firsttime {
			saveAllValues(uri, id)
			appendToCacheUsers(uri, msg.Chat.ID)
			setRssValue(buildKey("rss", uri, ""), "true")

			firsttime = false
			bot.Answer(msg).Text(fmt.Sprintf("You have been subscribed to %s", uri)).ReplyToMessage(msg.ID).End()
		}
		// Every MINS_SLEEP mins
		// <-time.After(time.Duration(feed.SecondsTillUpdate() * 1e9))
		<-time.After(time.Duration(MINS_SLEEP) * time.Minute)
	}
}

func getLastIdUrl(url string) (res string, found bool) {
	key := buildKey("last", url, "")
	val, ok := getRssValue(key)
	if ok {
		switch typed := val.(type) {
		case string:
			res = typed
			found = true
			return
		}
	}
	res = loadFromDb(key)
	found = res != ""

	if found {
		setRssValue(key, res)
	}
	return
}

func saveLastIdUrl(url string, items []*rss.Item) {
	if len(items) <= 0 {
		return
	}
	lastitem := items[0]
	newid := lastitem.Key()
	lastid, _ := getLastIdUrl(url)
	if newid != lastid {
		key := buildKey("last", url, "")
		// save it!!!
		saveInDb(map[string]string{key: newid})
		setRssValue(key, newid)
	}
}

func botItemHandler(bot tgbot.TgBot, firsttime bool) rss.ItemHandlerFunc {
	return func(feed *rss.Feed, ch *rss.Channel, newitems []*rss.Item) {
		// fmt.Printf("%d new item(s) in %s, firsttime: %v\n", len(newitems), feed.Url, firsttime)
		if firsttime {
			firsttime = false
			lastid, ok := getLastIdUrl(feed.Url)
			if ok {
				newitems = filterRssListByLastId(newitems, lastid)
			} else {
				saveLastIdUrl(feed.Url, newitems)
				return
			}
		}

		saveLastIdUrl(feed.Url, newitems)
		newst := ExtractNews(newitems)

		sendToAll(bot, feed.Url, newst)
	}
}

// Guess image format from gif/jpeg/png/webp
func guessImageFormat(r io.Reader) (format string, err error) {
	_, format, err = image.DecodeConfig(r)
	return
}

// Guess image mime types from gif/jpeg/png/webp
func guessImageMimeTypes(r io.Reader) string {
	format, err := guessImageFormat(r)
	if format == "" || err != nil {
		return ""
	}
	return mime.TypeByExtension("." + format)
}

func downloadImage(url string) (img interface{}, mimet string, err error) {
	resp, err := http.Get(url)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()

	byts, _ := ioutil.ReadAll(resp.Body)

	img, mimet, err = image.Decode(bytes.NewBuffer(byts))

	if strings.Contains(mimet, "gif") {
		img, err = gif.DecodeAll(bytes.NewBuffer(byts))
	}

	if err != nil {
		return
	}

	return
}

func sendToAll(bot tgbot.TgBot, uri string, newst []NewStruct) {
	var allones []int
	// In cache?
	x, found := getRssValue(uri)
	if found {
		switch val := x.(type) {
		case *set.Set:
			allones = set.IntSlice(val)
		default:
			fmt.Println("Error in type in rsscache: ", val)
		}
	} else {
		// Search in db and send
		urikey := buildKey("rss", uri, "")
		alloness := loadFromDbPrefix(urikey + ":")
		users := set.New()

		for key := range alloness {
			splitted := strings.Split(key, ":")
			i, e := strconv.Atoi(splitted[len(splitted)-1])
			if e == nil {
				allones = append(allones, i)
				users.Add(i)
			}
		}
		setRssValue(uri, users)
	}

	// Right now we are doing: for all user, send every new.
	// Maybe do it in the other way? For every new, send to all
	// Refactor this please ^^
	imagesids := make(map[string]string)
	gifsids := make(map[string]string)
	for _, i := range allones {
		useroptions := loadSettingsFromUser(i)

		for _, n := range newst {
			// Send text
			longSend(bot, i, n.BuildText())

			for _, link := range n.InternalLinks {
				bot.Send(i).Text(link).DisablePreview(false).End()
			}

			// handle options!
			if !useroptions.SendImage {
				continue
			}

			// Then images :)
			for _, im := range n.Images {
				// Search in cache
				id, ok := imagesids[im]
				if ok && id != "" {
					bot.Send(i).Photo(id).End()
					continue
				} else if ok && id == "" {
					continue
				}

				id, ok = gifsids[im]
				if ok && id != "" {
					bot.Send(i).Document(id).End()
					continue
				} else if ok && id == "" {
					continue
				}

				// If don't in cache, download, send and write in cache :)
				img, mimet, err := downloadImage(im)

				if err != nil {
					imagesids[im] = ""
					continue
				}
				if strings.Contains(mimet, "gif") || strings.Contains(mimet, "webp") {
					sended := bot.Send(i).Document(img).End()
					if sended.Ok && sended.Result.Document != nil {
						newimg := *sended.Result.Document
						gifsids[im] = newimg.FileID
					}

				} else {
					sended := bot.Send(i).Photo(img).End()
					if sended.Ok && sended.Result.Photo != nil && len(*sended.Result.Photo) > 0 {
						newimg := *sended.Result.Photo
						imagesids[im] = newimg[0].FileID
					}
				}
			}
		}
	}
}
