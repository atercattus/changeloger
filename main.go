package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"text/template"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/Masterminds/semver"
)

var (
	client = http.Client{}

	apiUrl string

	argv struct {
		GitHubToken string
		Help        bool
	}
)

const (
	NewTagPlaceholder = "NEW_TAG_HERE"
)

func init() {
	flag.StringVar(&argv.GitHubToken, "github-token", "", "OAuth2 token for GitHub API")
	flag.BoolVar(&argv.Help, "help", false, "Show this help")
	flag.BoolVar(&argv.Help, "h", false, "Show this help")

	flag.Parse()
}

type (
	MergeInfo struct {
		CommitHash     string
		UserName       string
		UserProfileUrl string
		UserEmail      string
		MergeNum       int
		MergeUrl       string
		Title          string
	}
)

func githubAPI(p string, dst interface{}) error {
	req, err := http.NewRequest("GET", apiUrl+p, nil)
	if err != nil {
		return fmt.Errorf(`can't create http request: %w`, err)
	}

	req.Header.Add("Accept", "application/vnd.github.v3+json")
	if argv.GitHubToken != "" {
		req.Header.Add("Authorization", "token "+argv.GitHubToken)
	}

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("can't get response: %w", err)
	}

	var buf bytes.Buffer
	_, err = buf.ReadFrom(resp.Body)
	resp.Body.Close()

	if err != nil {
		return fmt.Errorf("can't read response: %w", err)
	}
	respRaw := buf.String()

	err = json.NewDecoder(&buf).Decode(dst)
	if err != nil {
		return fmt.Errorf("can't parse response %q: %w", respRaw, err)
	}

	return nil
}

func execGit(dst io.Writer, args ...string) error {
	cmd := exec.Command("git", args...)
	cmd.Stdout = dst
	return cmd.Run()
}

func execGitOneLine(args ...string) (string, error) {
	var buf bytes.Buffer
	if err := execGit(&buf, args...); err != nil {
		return "", err
	}
	return strings.TrimSpace(buf.String()), nil
}

func setupRepoAPIURL() error {
	origin, err := execGitOneLine("config", "--get", "remote.origin.url")
	if err != nil {
		return err
	}

	parsed, err := url.Parse(origin)
	if err != nil {
		return err
	}

	if parsed.Host != "github.com" {
		return fmt.Errorf("only github.com repos are supported")
	}

	orgWithProject := strings.TrimSuffix(parsed.Path, ".git")
	if matched, err := regexp.MatchString("", orgWithProject); err != nil {
		return err
	} else if !matched {
		return fmt.Errorf("wrong repo name")
	}

	apiUrl = "https://api.github.com/repos" + orgWithProject + "/"

	return nil
}

func getLastGitTag() (string, error) {
	var buf bytes.Buffer
	if err := execGit(&buf, "tag"); err != nil {
		return "", fmt.Errorf("can't get git tag list: %w", err)
	}

	rdr := bufio.NewScanner(&buf)

	var (
		maxVer     *semver.Version
		maxVerAsIs string
	)
	for rdr.Scan() {
		curVer, err := semver.NewVersion(rdr.Text())
		if err != nil {
			log.Printf("Wrong semver tag %q: %s. Will ignore it.", rdr.Text(), err)
			continue
		}
		if maxVer == nil || curVer.GreaterThan(maxVer) {
			maxVer = curVer
			maxVerAsIs = rdr.Text()
		}
	}
	if rdr.Err() != nil {
		return "", fmt.Errorf("can't parse git tag list: %w", rdr.Err())
	}

	return maxVerAsIs, nil
}

func getGitMerges(lastTag string) ([]MergeInfo, error) {
	var buf bytes.Buffer
	if err := execGit(&buf, `log`, `--merges`, `--pretty=format:%H --- %an --- %aE --- %s`, lastTag+`..origin/master`); err != nil {
		return nil, err
	}

	rdr := bufio.NewScanner(&buf)

	mergeRegexp := regexp.MustCompile(`^Merge pull request #(\d+)`)

	var merges []MergeInfo

	for rdr.Scan() {
		line := strings.SplitN(rdr.Text(), " --- ", 4)
		if len(line) != 4 {
			log.Printf("Wrong merge line format %q. Will ignore it.", rdr.Text())
			continue
		}
		mergeText := line[3]

		idxs := mergeRegexp.FindStringSubmatchIndex(mergeText)
		if len(idxs) != 4 {
			log.Printf("Ignore merge %q", rdr.Text())
			continue
		}

		mergeNumber, err := strconv.Atoi(mergeText[idxs[2]:idxs[3]])
		if err != nil {
			// WTF?
			log.Printf("Wrong merge title format %q: %s. Will ignore it.", rdr.Text(), err)
			continue
		}

		merges = append(merges, MergeInfo{
			CommitHash: line[0],
			UserName:   line[1],
			UserEmail:  line[2],
			MergeNum:   mergeNumber,
		})
	}
	if rdr.Err() != nil {
		return nil, fmt.Errorf("can't parse git merges list: %w", rdr.Err())
	}

	sort.Slice(merges, func(i, j int) bool {
		return merges[i].MergeNum > merges[j].MergeNum
	})

	err := populateMergeWithInfo(merges)
	if err != nil {
		log.Printf("can't get GitHub names for some contributors: %s", err)
	}

	return merges, nil
}

func populateMergeWithInfo(merges []MergeInfo) error {
	for i := range merges {
		commitSha := merges[i].CommitHash

		var pullsInfo []struct {
			Url    string `json:"html_url"`
			Number int
			State  string
			Title  string
			User   struct {
				Login string
				Url   string `json:"html_url"`
			}
			// Body string
		}

		log.Printf("Get pull request by commit %s from %s...", commitSha, merges[i].UserName)

		err := githubAPI("commits/"+commitSha+"/pulls", &pullsInfo)
		if err != nil {
			log.Printf("Can't get pull requests by commit %s: %s", commitSha, err)
			continue
		}

		found := false
		for j := range pullsInfo {
			pull := &pullsInfo[j]
			if pull.State != "closed" {
				continue
			}

			merges[i].MergeNum = pull.Number
			merges[i].MergeUrl = pull.Url
			merges[i].Title = title(pull.Title)
			merges[i].UserName = pull.User.Login
			merges[i].UserProfileUrl = pull.User.Url

			found = true
			break
		}

		if !found {
			log.Printf("There is no closed PR found for merge %+v...", merges[i])
		}
	}

	return nil
}

func title(s string) string {
	if len(s) == 0 {
		return ""
	}

	r, size := utf8.DecodeRuneInString(s)

	if unicode.IsTitle(r) {
		return s
	}

	return string(unicode.ToTitle(r)) + s[size:]
}

func generateChangelogSection(merges []MergeInfo) (string, error) {
	const tpl = `
### Tag {{.Tag}} ({{.Date}})
{{range .Merges -}}
* {{.Title}}. [#{{.MergeNum}}]({{.MergeUrl}}) ([{{.UserName}}]({{.UserProfileUrl}}))
{{end -}}
`

	tmpl, err := template.New("changelog").Parse(tpl)
	if err != nil {
		return "", fmt.Errorf("can't compile template: %w", err)
	}

	fields := struct {
		Tag    string
		Date   string
		Merges []MergeInfo
	}{
		Tag:    NewTagPlaceholder,
		Date:   time.Now().Format("2006-01-02"),
		Merges: merges,
	}

	var buf bytes.Buffer
	err = tmpl.Execute(&buf, fields)
	if err != nil {
		return "", fmt.Errorf("can't build template: %w", err)
	}

	return buf.String(), nil
}

func generate() (string, error) {
	err := setupRepoAPIURL()
	if err != nil {
		return "", fmt.Errorf("can't get repo url: %w", err)
	}

	lastTag, err := getLastGitTag()
	if err != nil {
		return "", fmt.Errorf("can't get last repo tag: %w", err)
	}

	merges, err := getGitMerges(lastTag)
	if err != nil {
		return "", fmt.Errorf("can't get merges from last tag: %w", err)
	}

	cl, err := generateChangelogSection(merges)
	if err != nil {
		return "", fmt.Errorf("can't generate changelog: %w", err)
	}

	return cl, nil
}

func main() {
	if argv.Help {
		flag.PrintDefaults()
		return
	}

	changelog, err := generate()
	if err != nil {
		log.Fatalln(err)
	}

	fmt.Println(changelog)
}
