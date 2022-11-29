package main

import (
	"fmt"
	"io/ioutil"
	"log"
	"sync"

	"github.com/andygrunwald/go-jira"
)

var usernamecache = make(map[string]string)
var useridcache = make(map[string]string)
var usernamelock sync.Mutex

func userName(c *jira.Client, acctID string) string {
	usernamelock.Lock()
	defer usernamelock.Unlock()
	if name, ok := usernamecache[acctID]; ok {
		return name
	}
	user, _, err := c.User.Get(acctID)
	if err != nil {
		return acctID
	}
	usernamecache[acctID] = user.DisplayName
	useridcache[user.DisplayName] = acctID
	return user.DisplayName
}

func populateCache(c *jira.Client) {
	usernamelock.Lock()
	defer usernamelock.Unlock()
	var start int
	for {
		users, r, err := c.User.Find("", jira.WithStartAt(start), jira.WithMaxResults(1000))
		if err != nil {
			log.Printf("ERROR DOING TRANSITION: %v\n", err)
			defer r.Body.Close()
			bs, err := ioutil.ReadAll(r.Body)
			if err != nil {
				log.Printf("ERROR READING BODY: %v\n", err)
				return
			}
			log.Printf("BODY: [%s]\n", string(bs))
		}
		if len(users) == 0 {
			fmt.Printf("DONE\n")
			return
		}
		start += len(users)
		for _, u := range users {
			fmt.Printf("%v : %v\n", u.DisplayName, u.AccountID)
			usernamecache[u.AccountID] = u.DisplayName
			useridcache[u.DisplayName] = u.AccountID
		}
	}
}

func userID(c *jira.Client, name string) (string, error) {
	usernamelock.Lock()
	defer usernamelock.Unlock()
	id, ok := useridcache[name]
	if !ok {
		return "", fmt.Errorf("Didn't find id for user: %s\n")
	}
	return id, nil
}
