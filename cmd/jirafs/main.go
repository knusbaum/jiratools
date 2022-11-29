package main

import (
	"bufio"
	"bytes"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"os"
	"path"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/andygrunwald/go-jira"
	"github.com/keybase/go-keychain"
	"github.com/knusbaum/go9p"
	"github.com/knusbaum/go9p/fs"
	"github.com/knusbaum/go9p/proto"
	"golang.org/x/crypto/ssh/terminal"
)

const defaultSearchTTL = 5 * time.Minute // How often to refresh the issue search
const defaultIssueTTL = 5 * time.Minute  // How often to update an issue in the cache
//const defaultIssueHotcache = 8 * time.Hour // How long to keep an issue in the cache when not queried
const defaultIssueHotcache = 60 * time.Minute // How long to keep an issue in the cache when not queried

var (
	username string
	url      string
)

type IssueFile struct {
	fs.BaseFile
	field  string
	val    string
	opened map[uint64]string
}

type SearchDir struct {
	*fs.StaticDir
	FS     *fs.FS
	Client *jira.Client
	Query  string
	TTL    time.Duration

	updated time.Time
	cache   []jira.Issue
	sync.Mutex
}
type issueEntry struct {
	issue     *jira.Issue
	ttl       time.Time
	lastquery time.Time
}

var searchDirs = make(map[string]*SearchDir)
var searchDirsLock sync.RWMutex

var issueCache = make(map[string]issueEntry)
var issueCacheLock sync.RWMutex

func issueCacheLookup(key string) (*jira.Issue, bool) {
	issueCacheLock.RLock()
	defer issueCacheLock.RUnlock()
	if i, ok := issueCache[key]; ok && time.Now().Before(i.ttl) {
		log.Printf("Returning cached: %s\n", key)
		i.lastquery = time.Now()
		issueCache[key] = i
		return i.issue, ok
	}
	return nil, false
}

func issueCacheUpdateLastquery(key string) {
	issueCacheLock.RLock()
	defer issueCacheLock.RUnlock()
	if i, ok := issueCache[key]; ok {
		i.lastquery = time.Now()
		issueCache[key] = i
	}
}

func getIssue(c *jira.Client, key string) (*jira.Issue, error) {
	fmt.Printf("Querying issue %s.\n", key)
	newi, _, err := c.Issue.Get(key, &jira.GetQueryOptions{Expand: "changelog", Fields: "description,summary,assignee,parent,issuelinks,sub-tasks,status,updated,comment,labels"})
	if err != nil {
		fmt.Printf("Failed to get issue: %s\n", err)
		return nil, err
	}
	issueCacheLock.Lock()
	defer issueCacheLock.Unlock()
	var issue issueEntry
	if i, ok := issueCache[key]; ok {
		issue = i
		issue.issue = newi
		issue.ttl = time.Now().Add(defaultIssueTTL)
	} else {
		issue = issueEntry{issue: newi, ttl: time.Now().Add(defaultIssueTTL), lastquery: time.Unix(0, 0)}
	}
	issueCache[key] = issue
	return newi, nil
}

func lookupIssue(c *jira.Client, key string) (*jira.Issue, error) {
	if i, ok := issueCacheLookup(key); ok {
		return i, nil
	}
	defer issueCacheUpdateLastquery(key)
	return getIssue(c, key)
}

func invalidateCachedIssue(key string) {
	issueCacheLock.Lock()
	defer issueCacheLock.Unlock()
	if i, ok := issueCache[key]; ok {
		i.ttl = time.Unix(0, 0)
		issueCache[key] = i
	}
}

func removeCachedIssue(key string) {
	issueCacheLock.Lock()
	issueCacheLock.Unlock()
	delete(issueCache, key)
}

func purgeIssueCache() {
	issueCacheLock.Lock()
	issueCacheLock.Unlock()
	issueCache = make(map[string]issueEntry)
}

func invalidateDirCache() {
	searchDirsLock.RLock()
	defer searchDirsLock.RUnlock()
	for _, dir := range searchDirs {
		dir.Invalidate()
	}
}

func purgeCache() {
	purgeIssueCache()
	invalidateDirCache()
}

// hotloop keeps the jira issue cache hot.
func hotloop(c *jira.Client) {
	for {
		time.Sleep(1 * time.Second)
		// Update stale issues
		toUpdate := make([]string, 0)
		toDelete := make([]string, 0)
		func() {
			issueCacheLock.RLock()
			defer issueCacheLock.RUnlock()
			for key, issue := range issueCache {
				//fmt.Printf("Last query for %s: %v\n", key, issue.lastquery)
				if time.Now().After(issue.lastquery.Add(defaultIssueHotcache)) {
					toDelete = append(toDelete, key)
					continue
				}
				if time.Now().After(issue.ttl) {
					toUpdate = append(toUpdate, key)
				}
			}
		}()
		for _, key := range toDelete {
			fmt.Printf("Issue [%s] falling out of hot cache.\n", key)
			removeCachedIssue(key)
		}
		for _, key := range toUpdate {
			go getIssue(c, key)
		}

		go func() {
			// Update stale searchdirs
			searchDirsLock.RLock()
			defer searchDirsLock.RUnlock()
			for _, dir := range searchDirs {
				dir.Update()
			}
		}()
	}
}

func makeIssueDir(jiraFS *fs.FS, c *jira.Client, stat proto.Stat, key string) *fs.StaticDir {
	subdir := fs.NewStaticDir(jiraFS.NewStat(key, stat.Uid, stat.Gid, stat.Mode))
	subdir.AddChild(NewCachingDynamicFile(jiraFS.NewStat("description", stat.Uid, stat.Gid, 0444), func() []byte {
		// 		newi, _, err := c.Issue.Get(key, &jira.GetQueryOptions{Fields: "description"})
		newi, err := lookupIssue(c, key)
		if err != nil {
			fmt.Printf("Failed to get issue: %s\n", err)
			return []byte("\n")
		}
		re := regexp.MustCompile(`\[~accountid:([0-9a-z]*)\]`)
		matches := re.FindAllStringSubmatch(newi.Fields.Description, -1)
		replacements := make(map[string]string)
		for i := range matches {
			replacements[matches[i][0]] = "@" + userName(c, matches[i][1])
		}
		description := newi.Fields.Description
		for k, v := range replacements {
			description = strings.ReplaceAll(description, k, v)
		}
		return []byte(description + "\n")
	}))
	subdir.AddChild(NewCachingDynamicFile(jiraFS.NewStat("summary", stat.Uid, stat.Gid, 0444), func() []byte {
		// 		newi, _, err := c.Issue.Get(key, &jira.GetQueryOptions{Fields: "summary"})
		newi, err := lookupIssue(c, key)
		if err != nil {
			fmt.Printf("Failed to get issue: %s\n", err)
			return []byte("\n")
		}
		//fmt.Printf("Issue: %s [%s] - %s\n", newi.Key, key, newi.Fields.Summary)
		return []byte(newi.Fields.Summary + "\n")
	}))
	subdir.AddChild(NewCachingDynamicFile(jiraFS.NewStat("assignee", stat.Uid, stat.Gid, 0444), func() []byte {
		// 		newi, _, err := c.Issue.Get(key, &jira.GetQueryOptions{Fields: "assignee"})
		newi, err := lookupIssue(c, key)
		if err != nil {
			fmt.Printf("Failed to get issue: %s\n", err)
			return []byte("\n")
		}
		if newi.Fields != nil && newi.Fields.Assignee != nil {
			//fmt.Printf("Issue: %s [%s] - %s\n", newi.Key, key, newi.Fields.Assignee.DisplayName)
			return []byte(newi.Fields.Assignee.DisplayName + "\n")
		}
		//fmt.Printf("Issue: %s [%s] - NO ASSIGNEE\n", newi.Key, key)
		return []byte("\n")
	}))
	//	It seems the "epic" field is no longer used, and is "parent" now instead.
	// 	subdir.AddChild(NewCachingDynamicFile(jiraFS.NewStat("epic", stat.Uid, stat.Gid, 0444), func() []byte {
	// 		//newi, _, err := c.Issue.Get(key, &jira.GetQueryOptions{Fields: "epic"})
	// 		newi, _, err := c.Issue.Get(key, nil)
	// 		if err != nil {
	// 			fmt.Printf("Failed to get issue: %s\n", err)
	// 			return nil
	// 		}
	// 		if newi.Fields != nil && newi.Fields.Epic != nil {
	// 			fmt.Printf("Issue: %s [%s] - %s\n", newi.Key, key, newi.Fields.Epic.Key)
	// 			return []byte(newi.Fields.Epic.Key + "\n")
	// 		}
	// 		fmt.Printf("Issue: %s [%s] - NO EPIC\n", newi.Key, key)
	// 		return []byte("\n")
	// 	}))
	subdir.AddChild(NewCachingDynamicFile(jiraFS.NewStat("parent", stat.Uid, stat.Gid, 0444), func() []byte {
		// 		newi, _, err := c.Issue.Get(key, &jira.GetQueryOptions{Fields: "parent"})
		newi, err := lookupIssue(c, key)
		if err != nil {
			fmt.Printf("Failed to get issue: %s\n", err)
			return []byte("\n")
		}
		if newi.Fields != nil && newi.Fields.Parent != nil {
			//fmt.Printf("Issue: %s [%s] - %s\n", newi.Key, key, newi.Fields.Parent.Key)
			return []byte(newi.Fields.Parent.Key + "\n")
		}
		//fmt.Printf("Issue: %s [%s] - NO PARENT\n", newi.Key, key)
		return []byte("\n")
	}))
	subdir.AddChild(NewCachingDynamicFile(jiraFS.NewStat("links", stat.Uid, stat.Gid, 0444), func() []byte {
		// 		newi, _, err := c.Issue.Get(key, &jira.GetQueryOptions{Fields: "issuelinks"})
		newi, err := lookupIssue(c, key)
		if err != nil {
			fmt.Printf("Failed to get issue: %s\n", err)
			return []byte("\n")
		}
		if newi.Fields != nil && newi.Fields.IssueLinks != nil {
			var b bytes.Buffer
			for _, link := range newi.Fields.IssueLinks {
				var outkey string
				var inkey string
				if link.OutwardIssue != nil {
					outkey = link.OutwardIssue.Key
				}
				if link.InwardIssue != nil {
					inkey = link.InwardIssue.Key
				}
				//fmt.Printf("Issue: %s [%s] - %s -> %s\n", newi.Key, key, outkey, inkey)
				b.WriteString(fmt.Sprintf("%s -> %s\n", outkey, inkey))
			}
			return b.Bytes()
		}
		//fmt.Printf("Issue: %s [%s] - NO LINKS\n", newi.Key, key)
		return []byte("\n")
	}))
	subdir.AddChild(NewCachingDynamicFile(jiraFS.NewStat("subtasks", stat.Uid, stat.Gid, 0444), func() []byte {
		//newi, _, err := c.Issue.Get(key, &jira.GetQueryOptions{Fields: "sub-tasks"})
		newi, _, err := c.Issue.Get(key, nil)
		if err != nil {
			fmt.Printf("Failed to get issue: %s\n", err)
			return []byte("\n")
		}
		if newi.Fields != nil && newi.Fields.Subtasks != nil {
			var b bytes.Buffer
			for _, task := range newi.Fields.Subtasks {
				//fmt.Printf("Issue: %s [%s] - %s\n", newi.Key, key, task.Key)
				b.WriteString(fmt.Sprintf("%s\n", task.Key))
			}
			return b.Bytes()
		}
		//fmt.Printf("Issue: %s [%s] - NO LINKS\n", newi.Key, key)
		return []byte("\n")
	}))
	subdir.AddChild(NewCachingDynamicFile(jiraFS.NewStat("status", stat.Uid, stat.Gid, 0444), func() []byte {
		// 		newi, _, err := c.Issue.Get(key, &jira.GetQueryOptions{Fields: "status"})
		newi, err := lookupIssue(c, key)
		if err != nil {
			fmt.Printf("Failed to get issue: %s\n", err)
			return []byte("\n")
		}
		if newi.Fields != nil && newi.Fields.Status != nil {
			var b bytes.Buffer
			status := newi.Fields.Status
			//fmt.Printf("Issue: %s [%s] - %s, %s, %s, %s\n", newi.Key, key, status.Self, status.Description, status.Name, status.ID)
			b.WriteString(fmt.Sprintf("%s\n", status.Name))
			return b.Bytes()
		}
		//fmt.Printf("Issue: %s [%s] - NO STATUS\n", newi.Key, key)
		return []byte("\n")
	}))
	subdir.AddChild(NewCachingDynamicFile(jiraFS.NewStat("updated", stat.Uid, stat.Gid, 0444), func() []byte {
		// 		newi, _, err := c.Issue.Get(key, &jira.GetQueryOptions{Fields: "updated"})
		newi, err := lookupIssue(c, key)
		if err != nil {
			fmt.Printf("Failed to get issue: %s\n", err)
			return []byte("\n")
		}
		if newi.Fields != nil {
			var b bytes.Buffer
			updated := time.Time(newi.Fields.Updated)
			//fmt.Printf("Issue: %s [%s] - %s\n", newi.Key, key, updated.Format("Jan 02, 2006 (3:04 MST)"))
			b.WriteString(fmt.Sprintf("%s\n", updated.Format("Jan 02, 2006 (3:04 MST)")))
			return b.Bytes()
		}
		//fmt.Printf("Issue: %s [%s] - NO STATUS\n", newi.Key, key)
		return []byte("\n")
	}))
	subdir.AddChild(NewCachingDynamicFile(jiraFS.NewStat("history", stat.Uid, stat.Gid, 0444), func() []byte {
		//newi, _, err := c.Issue.Get(key, &jira.GetQueryOptions{Expand: "changelog"})
		//log.Printf("Querying issue changelog %s\n", key)
		newi, err := lookupIssue(c, key)
		if err != nil {
			fmt.Printf("Failed to get issue: %s\n", err)
			return []byte("\n")
		}
		if newi.Changelog != nil {
			var b bytes.Buffer
			for _, history := range newi.Changelog.Histories {
				t, err := history.CreatedTime()
				if err != nil {
					continue
				}
				fmt.Fprintf(&b, "Author: %s - %s:\n", history.Author.DisplayName, t.Format("Jan 02, 2006 (3:04 MST)"))
				for _, item := range history.Items {
					fmt.Fprintf(&b, "\t%s:  %s -> %s\n", item.Field, item.FromString, item.ToString)
				}
			}
			return b.Bytes()
		}
		fmt.Printf("Issue: %s [%s] - NO Changelog\n", newi.Key, key)
		return []byte("\n")
	}))
	subdir.AddChild(NewCachingDynamicFile(jiraFS.NewStat("comments", stat.Uid, stat.Gid, 0444), func() []byte {
		// 		newi, _, err := c.Issue.Get(key, &jira.GetQueryOptions{Fields: "comment"})
		newi, err := lookupIssue(c, key)
		if err != nil {
			fmt.Printf("Failed to get issue: %s\n", err)
			return []byte("\n")
		}
		if newi.Fields != nil && newi.Fields.Comments != nil {
			var b bytes.Buffer
			re := regexp.MustCompile(`\[~accountid:([0-9a-z]*)\]`)
			for _, comment := range newi.Fields.Comments.Comments {
				matches := re.FindAllStringSubmatch(comment.Body, -1)
				replacements := make(map[string]string)
				for i := range matches {
					replacements[matches[i][0]] = "@" + userName(c, matches[i][1])
				}
				body := comment.Body
				for k, v := range replacements {
					body = strings.ReplaceAll(body, k, v)
				}
				fmt.Fprintf(&b, "########################################\n\n%s (%s):\n%s\n\n", comment.UpdateAuthor.DisplayName, comment.Updated, body)
			}
			return b.Bytes()
		}
		//fmt.Printf("Issue: %s [%s] - NO Changelog\n", newi.Key, key)
		return []byte("\n")
	}))
	subdir.AddChild(NewCachingDynamicFile(jiraFS.NewStat("labels", stat.Uid, stat.Gid, 0444), func() []byte {
		// 		newi, _, err := c.Issue.Get(key, &jira.GetQueryOptions{Fields: "labels"})
		newi, err := lookupIssue(c, key)
		if err != nil {
			fmt.Printf("Failed to get issue: %s\n", err)
			return []byte("\n")
		}
		if newi.Fields != nil && newi.Fields.Labels != nil {
			var b bytes.Buffer
			for _, l := range newi.Fields.Labels {
				fmt.Fprintf(&b, "%s\n", l)
			}
			return b.Bytes()
		}
		//fmt.Printf("Issue: %s [%s] - NO Changelog\n", newi.Key, key)
		return []byte("\n")
	}))

	notes := path.Join(notesDir(), key)
	f, err := os.OpenFile(notes, os.O_CREATE, 0660)
	if err != nil {
		fmt.Printf("Failed to create notes file: %s", err)
	} else {
		f.Close()
		subdir.AddChild(NewRealFile("notes", notes, subdir))
	}
	return subdir
}

func configDir() string {
	configDir, err := os.UserConfigDir()
	if err != nil {
		panic(fmt.Sprintf("Failed to get user config directory: %s", err))
	}
	jfsdir := path.Join(configDir, "jirafs")

	err = os.MkdirAll(jfsdir, os.FileMode(0770))
	if err != nil {
		panic(fmt.Sprintf("Failed to create jirafs config directory: %s", err))
	}
	return jfsdir
}

func notesDir() string {
	notesdir := path.Join(configDir(), "notes")
	err := os.MkdirAll(notesdir, os.FileMode(0770))
	if err != nil {
		panic(fmt.Sprintf("Failed to create jirafs config directory: %s", err))
	}
	return notesdir
}

func (d *SearchDir) Children() map[string]fs.FSNode {
	err := d.Update()
	if err != nil {
		fmt.Printf("Failed to get issues: %s\n", err)
		return nil
	}
	issues := d.cache
	m := make(map[string]fs.FSNode)
	for _, issue := range issues {
		m[issue.Key] = makeIssueDir(d.FS, d.Client, d.Stat(), issue.Key)
	}
	m["query"] = fs.NewStaticFile(
		d.FS.NewStat("query", d.Stat().Uid, d.Stat().Gid, 0444),
		[]byte(d.Query+"\n"),
	)
	return m
}

func (d *SearchDir) Invalidate() {
	d.Lock()
	defer d.Unlock()
	d.updated = time.Unix(0, 0)
}

func (d *SearchDir) Update() error {
	d.Lock()
	defer d.Unlock()
	if d.updated.Add(d.TTL).Before(time.Now()) {
		fmt.Printf("Updating Search... (%s)\n", d.Query)
		issues, _, err := d.Client.Issue.Search(d.Query, &jira.SearchOptions{MaxResults: 500, Fields: []string{"summary"}})
		if err != nil {
			return err
		}
		d.updated = time.Now()
		d.cache = issues
	}
	return nil
}

func init() {
	flag.StringVar(&username, "username", "", "The Jira username to login with")
	flag.StringVar(&url, "url", "", "The base Jira URL")
}

type VerboseRT struct {
	inner http.RoundTripper
}

func (v *VerboseRT) RoundTrip(req *http.Request) (*http.Response, error) {
	req.Write(os.Stdout)
	resp, err := v.inner.RoundTrip(req)
	if err != nil {
		fmt.Printf("ERROR: %s\n", err)
		return resp, err
	}
	fmt.Printf("##########\n")
	resp.Write(os.Stdout)
	fmt.Printf("##########\n")
	return resp, err
}

func handleCtl(jiraFS *fs.FS, root *fs.StaticDir, client *jira.Client, newFile *fs.ListenFile) {
	l := (*fs.ListenFileListener)(newFile)
	defer l.Close()
	for {
		conn, err := l.Accept()
		if err != nil {
			log.Printf("FAIL: %s\n", err)
			return
		}
		go func(conn net.Conn) {
			log.Printf("Handling connection.\n")
			defer log.Printf("Finished connection.\n")
			defer conn.Close()
			r := bufio.NewReader(conn)
			for {
				line, err := r.ReadString('\n')
				if err != nil {
					log.Printf("Failed to read: %v\n", err)
					return
				}
				splits := strings.SplitN(line, ":", 2)
				command := strings.ToLower(strings.TrimSpace(splits[0]))
				var args string
				if len(splits) > 1 {
					args = strings.TrimSpace(splits[1])
				}
				switch command {
				case "setstatus":
					argsplit := strings.SplitN(args, " ", 2)
					if len(argsplit) != 2 {
						log.Printf("Error: Must specify an Issue and a status.")
						return
					}
					issueName := strings.TrimSpace(argsplit[0])
					args = strings.TrimSpace(argsplit[1])
					log.Printf("Setting Status on [%s] to [%s]\n", issueName, args)
					err := setStatus(client, issueName, args)
					if err != nil {
						log.Printf("Error: %v\n", err)
					}
				case "tag":
					argsplit := strings.SplitN(args, " ", 2)
					if len(argsplit) != 2 {
						log.Printf("Error: Must specify an Issue and a tag.")
						return
					}
					issueName := strings.TrimSpace(argsplit[0])
					args = strings.TrimSpace(argsplit[1])
					log.Printf("Tagging [%s] with [%s]\n", issueName, args)
					err := addLabel(client, issueName, args)
					if err != nil {
						log.Printf("Error: %v\n", err)
					}
				case "untag":
					argsplit := strings.SplitN(args, " ", 2)
					if len(argsplit) != 2 {
						log.Printf("Error: Must specify an Issue and a tag.")
						return
					}
					issueName := strings.TrimSpace(argsplit[0])
					args = strings.TrimSpace(argsplit[1])
					log.Printf("Untagging [%s] with [%s]\n", issueName, args)
					err := removeLabel(client, issueName, args)
					if err != nil {
						log.Printf("Error: %v\n", err)
					}
				case "assign":
					argsplit := strings.SplitN(args, " ", 2)
					if len(argsplit) != 2 {
						log.Printf("Error: Must specify an Issue and a tag.")
						return
					}
					issueName := strings.TrimSpace(argsplit[0])
					args = strings.TrimSpace(argsplit[1])
					log.Printf("Assigning [%s] to [%s]\n", issueName, args)
					err := setUser(client, issueName, args)
					if err != nil {
						log.Printf("Error: %v\n", err)
					}
				case "rmdir":
					log.Printf("Removing [%s]\n", args)
					root.DeleteChild(args)
					searchDirsLock.Lock()
					delete(searchDirs, args)
					searchDirsLock.Unlock()
				case "rmall":
					for k, n := range root.Children() {
						if _, ok := n.(fs.Dir); ok {
							log.Printf("Removing [%s]\n", k)
							root.DeleteChild(k)
						}
					}
					searchDirsLock.Lock()
					searchDirs = make(map[string]*SearchDir)
					searchDirsLock.Unlock()
				case "purge":
					purgeCache()
				default:
					log.Printf("Error: unknown command %s\n", command)
				}
			}
		}(conn)
	}
}

func handleNewFiles(jiraFS *fs.FS, root *fs.StaticDir, client *jira.Client, newFile *fs.ListenFile) {
	l := (*fs.ListenFileListener)(newFile)
	defer l.Close()
	for {
		conn, err := l.Accept()
		if err != nil {
			log.Printf("FAIL: %s\n", err)
			return
		}
		go func(conn net.Conn) {
			defer conn.Close()
			bs, err := ioutil.ReadAll(conn)
			if err != nil {
				log.Printf("Failed to read: %v\n", err)
				return
			}
			log.Printf("Received: [%s]\n", string(bs))
			lines := strings.Split(string(bs), "\n")
			m := make(map[string]string)
			//m["ttl"] = "10"
			for _, line := range lines {
				splits := strings.SplitN(line, ":", 2)
				if len(splits) < 2 {
					log.Printf("Failed to parse line [%s]\n", line)
					continue
				}
				key := strings.ToLower(strings.TrimSpace(splits[0]))
				val := strings.TrimSpace(splits[1])
				m[key] = val
			}
			log.Printf("Received config: %v\n", m)
			if _, ok := m["name"]; !ok {
				log.Printf("Error: Expected key \"name\"")
				return
			}
			if _, ok := m["query"]; !ok {
				log.Printf("Error: Expected key \"query\"")
				return
			}
			var ttlDuration time.Duration
			ttl, err := strconv.ParseInt(m["ttl"], 10, 32)
			if err != nil {
				ttlDuration = defaultSearchTTL
				log.Printf("Invalid TTL: %s, defaulting to %d Seconds.", err, defaultSearchTTL/time.Second)
			} else {
				ttlDuration = time.Duration(ttl) * time.Second
			}
			dir := &SearchDir{
				StaticDir: fs.NewStaticDir(jiraFS.NewStat(m["name"], username, username, 0555)),
				FS:        jiraFS,
				Client:    client,
				Query:     m["query"],
				TTL:       ttlDuration,
			}
			root.AddChild(dir)
			searchDirsLock.Lock()
			defer searchDirsLock.Unlock()
			searchDirs[m["name"]] = dir
		}(conn)
	}
}

var ErrNotExist = fmt.Errorf("No such keychain entry.")

func addKeychainPW(username, password string) error {
	item := keychain.NewGenericPassword("jirafs", username, "jirafs", []byte(password), "jirafs")
	return keychain.AddItem(item)
}

func getKeychainPW(username string) (string, error) {
	pw, err := keychain.GetGenericPassword("jirafs", username, "jirafs", "jirafs")
	if err != nil {
		return "", err
	} else if pw == nil {
		return "", ErrNotExist
	}
	return string(pw), nil
}

func getTermPW(username string) (string, error) {
	fmt.Printf("Password for %s: ", username)
	passb, err := terminal.ReadPassword(0)
	if err != nil {
		fmt.Printf("Error: %s\n", err)
		return "", err
	}
	pass := string(passb)
	fmt.Printf("\n")
	return pass, nil
}

func main() {
	flag.Parse()
	if username == "" {
		fmt.Println("Error: Username not specified.")
		flag.PrintDefaults()
		return
	}
	if url == "" {
		fmt.Println("Error: URL not specified.")
		flag.PrintDefaults()
		return
	}

	pass, err := getKeychainPW(username) //getTermPW(username)
	if err != nil {
		if err == ErrNotExist {
			pass, err = getTermPW(username)
			if err != nil {
				log.Printf("Failed to get password: %s", err)
				os.Exit(1)
			}
			addKeychainPW(username, pass)
		} else {
			log.Printf("Failed to get password: %s", err)
			os.Exit(1)
		}
	}

	// Start Jira
	tp := &jira.BasicAuthTransport{
		Username: username,
		Password: pass,
	}

	client, err := jira.NewClient(&http.Client{Transport: tp}, url)
	if err != nil {
		log.Printf("Failed to create client: %s\n", err)
		return
	}

	go populateCache(client)

	// Disable the hot cache.
	go hotloop(client)

	jiraFS, root := fs.NewFS("kyle.nusbaum", "kyle.nusbaum", 0777)
	newFile := fs.NewListenFile(jiraFS.NewStat("new", "glenda", "glenda", 0666))
	root.AddChild(newFile)
	go handleNewFiles(jiraFS, root, client, newFile)
	ctlFile := fs.NewListenFile(jiraFS.NewStat("ctl", "glenda", "glenda", 0666))
	root.AddChild(ctlFile)
	go handleCtl(jiraFS, root, client, ctlFile)
	root.AddChild(fs.NewDynamicFile(jiraFS.NewStat("clear_cache", "glenda", "glenda", 0444), func() []byte {
		issueCacheLock.Lock()
		defer issueCacheLock.Unlock()
		issueCache = make(map[string]issueEntry)
		return []byte("OK\n")
	}))
	fmt.Printf("Starting jirafs\n")
	//go9p.PostSrv("jira", jiraFS.Server())
	//go9p.Verbose = true
	log.Printf("Finished: %v", go9p.Serve("localhost:10000", jiraFS.Server()))
}

func setStatus(client *jira.Client, issue string, status string) error {
	var id string
	transitions, _, err := client.Issue.GetTransitions(issue)
	if err != nil {
		return err
	}
	for _, t := range transitions {
		//log.Printf("Transition [%s] -> %s\n", t.Name, t.To.Name)
		// EqualFold is unicode's case-insensitivity solution
		if strings.EqualFold(status, t.To.Name) {
			id = t.ID
			break
		}
	}
	if id == "" {
		return fmt.Errorf("No such transition for %s", status)
	}
	defer invalidateCachedIssue(issue)
	defer invalidateDirCache()
	r, err := client.Issue.DoTransition(issue, id)
	if err != nil {
		log.Printf("ERROR DOING TRANSITION: %v\n", err)
		defer r.Body.Close()
		bs, err := ioutil.ReadAll(r.Body)
		if err != nil {
			log.Printf("ERROR READING BODY: %v\n", err)
			return err
		}
		log.Printf("BODY: [%s]\n", string(bs))
	}
	return err
}

func addLabel(client *jira.Client, issue string, tag string) error {
	defer invalidateCachedIssue(issue)
	defer invalidateDirCache()
	r, err := client.Issue.UpdateIssue(issue, map[string]interface{}{
		"update": map[string]interface{}{
			"labels": []map[string]string{
				{"add": tag},
			},
		},
	})
	if err != nil {
		log.Printf("ERROR ADDING LABEL: %v\n", err)
		defer r.Body.Close()
		bs, err := ioutil.ReadAll(r.Body)
		if err != nil {
			log.Printf("ERROR READING BODY: %v\n", err)
			return err
		}
		log.Printf("BODY: [%s]\n", string(bs))
	}
	return err
}

func removeLabel(client *jira.Client, issue string, tag string) error {
	defer invalidateCachedIssue(issue)
	defer invalidateDirCache()
	_, err := client.Issue.UpdateIssue(issue, map[string]interface{}{
		"update": map[string]interface{}{
			"labels": []map[string]string{
				{"remove": tag},
			},
		},
	})
	return err
}

func setUser(client *jira.Client, issue string, user string) error {
	defer invalidateCachedIssue(issue)
	defer invalidateDirCache()
	// 	r, err := client.Issue.UpdateIssue(issue, map[string]interface{}{
	// 		"update": map[string]interface{}{
	// 			"name": user,
	// 		},
	// 	})
	id, err := userID(client, user)
	if err != nil {
		log.Printf("ERROR Assigning %s to %s: %v\n", issue, user, err)
		return err
	}
	r, err := client.Issue.UpdateAssignee(issue, &jira.User{AccountID: id})
	if err != nil {
		log.Printf("ERROR Assigning %s to %s: %v\n", issue, user, err)
		defer r.Body.Close()
		bs, err := ioutil.ReadAll(r.Body)
		if err != nil {
			log.Printf("ERROR READING BODY: %v\n", err)
			return err
		}
		log.Printf("BODY: [%s]\n", string(bs))
	}
	return err
}

func old_main() {
	flag.Parse()
	if username == "" {
		fmt.Println("Error: Username not specified.")
		flag.PrintDefaults()
		return
	}
	if url == "" {
		fmt.Println("Error: URL not specified.")
		flag.PrintDefaults()
		return
	}
	// 	userPwd, err := libauth.Getuserpasswd("proto=pass service=jira user=%s server=%s role=client", username, url)
	// 	if err != nil {
	// 		fmt.Printf("Error: %s\n", err)
	// 		return
	// 	}
	// 	pass := userPwd.Password

	fmt.Printf("Password for %s: ", username)
	passb, err := terminal.ReadPassword(0)
	if err != nil {
		fmt.Printf("Error: %s\n", err)
		return
	}
	pass := string(passb)

	// Start Jira
	tp := &jira.BasicAuthTransport{
		Username: username,
		Password: pass,
		//Transport: &VerboseRT{http.DefaultTransport},
	}

	client, err := jira.NewClient(&http.Client{Transport: tp}, url)
	if err != nil {
		fmt.Printf("Failed to create client: %s\n", err)
		return
	}

	issue, _, err := client.Issue.Get("APMGO-98", nil)
	if err != nil {
		fmt.Printf("Failed to get issue: %s\n", err)
		return
	}
	fmt.Printf("Issue: %s - %s\n", issue.ID, issue.Fields.Description)

	issues, _, err := client.Issue.Search("assignee = currentUser() AND resolution = Unresolved order by updated ASC", &jira.SearchOptions{Fields: []string{"comment", "summary"}})
	if err != nil {
		fmt.Printf("Failed to get issues: %s\n", err)
		return
	}
	for _, issue := range issues {
		//fmt.Printf("ISSUE: %#v\n", issue)
		fmt.Printf("%s - %s\n", issue.Key, issue.Fields.Summary)
		if issue.Fields.Comments != nil {
			last := len(issue.Fields.Comments.Comments) - 1
			if last < 0 {
				continue
			}
			comment := issue.Fields.Comments.Comments[last]
			fmt.Printf("\t[%s]: %s\n", comment.Author.DisplayName, comment.Body)
			// 			for _, comment := range issue.Fields.Comments.Comments {
			// 				if comment == nil {
			// 					fmt.Printf("COMMENT: <nil>")
			// 					continue
			// 				}
			// 				fmt.Printf("\t%s\n", comment.Body)
			// 			}
		}
	}
}

// func query(jql string) ([]jira.Issue, error)
