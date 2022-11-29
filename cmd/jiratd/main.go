package main

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"os"
	"path"
	"regexp"
	"strings"
)

func main() {
	var (
		line string
		err  error
	)
	rdr := bufio.NewReader(os.Stdin)
	re := regexp.MustCompile(`^([A-Z]+-[0-9]+)`)
	// Prime the reader
	for {
		if re.FindStringIndex(line) != nil {
			break
		}
		line, err = rdr.ReadString('\n')
		if err != nil {
			if err == io.EOF {
				fmt.Fprintf(os.Stderr, "EOF WITH: %s\n", line)
				return
			}
			fmt.Fprintf(os.Stderr, "Error: %s\n", err)
			os.Exit(1)
		}
	}

	for ; line != ""; {
		fmt.Fprintf(os.Stderr, "Find String [%s]\n", line)
		jira := re.FindString(line)
		var bs bytes.Buffer
		for {
			line, err = rdr.ReadString('\n')
			if err != nil {
				if err == io.EOF {
					fmt.Fprintf(os.Stderr, "EOF WITH: %s\n", line)
					break
				}
				fmt.Fprintf(os.Stderr, "Error: %s\n", err)
				os.Exit(1)
			}
			if strings.HasPrefix(line, "#") {
				continue
			}
			if re.FindStringIndex(line) != nil {
				break
			}
			bs.WriteString(line)
		}
		fmt.Fprintf(os.Stderr, "JIRA: [%s] NOTES: [\n%s]\n", jira, bs.String())
		f, err := os.OpenFile(path.Join("/Users/kyle.nusbaum/Library/Application Support/jirafs/notes", jira), os.O_WRONLY|os.O_TRUNC, 0660)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error writing notes for %s: %s\n", jira, err)
			continue
		}
		s := bs.String()
		re := regexp.MustCompile("\n+$")
		s = re.ReplaceAllString(s, "\n")
		//s = strings.TrimSuffix(s, "\n ")
		//io.Copy(f, &bs)
		f.WriteString(s)
		f.Close()
	}
}
