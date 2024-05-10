package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os/exec"
	"regexp"
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
		MainBranch  string
	}
)

const (
	NewTagPlaceholder = "NEW_TAG_HERE"
)

var gitUrlRegex = regexp.MustCompile("^git@([^:]+):([^/]+)/([^/]+).git$")

func init() {
	flag.StringVar(&argv.GitHubToken, "github-token", "", "OAuth2 token for GitHub API")
	flag.StringVar(&argv.MainBranch, "main-branch", "main", "Name of the main branch (main, master, ...)")
	flag.BoolVar(&argv.Help, "help", false, "Show this help")
	flag.BoolVar(&argv.Help, "h", false, "Show this help")

	flag.Parse()
}

type (
	MergeInfo struct {
		CommitHash     string
		UserName       string
		UserProfileUrl string
		//UserEmail      string
		MergeNum int
		MergeUrl string
		Title    string
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
	log.Println("#", cmd.String())
	cmd.Stdout = dst
	var buf bytes.Buffer
	cmd.Stderr = &buf
	err := cmd.Run()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return fmt.Errorf("%s: %s", ee.String(), buf.String())
		} else {
			log.Println(buf.String())
		}
	}
	return err
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

	if m := gitUrlRegex.FindAllStringSubmatch(origin, -1); len(m) > 0 {
		origin = "git://" + strings.Join(m[0][1:4], "/")
	}

	parsed, err := url.Parse(origin)
	if err != nil {
		return fmt.Errorf("url parse: %w", err)
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
	log.Println("apiUrl:", apiUrl)

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
	var pullsInfo []struct {
		Url    string `json:"html_url"`
		Number int
		State  string
		Title  string
		User   struct {
			Login string
			Url   string `json:"html_url"`
		}
		MergeCommitSHA string `json:"merge_commit_sha"`
		MergedAt       string `json:"merged_at"`
		// Body string
	}

	err := githubAPI("pulls?state=closed&sort=updated&direction=desc&per_page=100", &pullsInfo)
	if err != nil {
		return nil, fmt.Errorf("get pulls from githubAPI: %w", err)
	}

	var merges []MergeInfo
	for _, pi := range pullsInfo {
		if pi.MergedAt == "" {
			continue
		}

		merges = append(merges, MergeInfo{
			CommitHash:     pi.MergeCommitSHA,
			UserName:       pi.User.Login,
			UserProfileUrl: pi.User.Url,
			MergeNum:       pi.Number,
			MergeUrl:       pi.Url,
			Title:          title(pi.Title),
		})
	}

	return merges, nil
}

func title(s string) string {
	s = strings.TrimSpace(s)

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

	//lastTag, err := getLastGitTag()
	//if err != nil {
	//	return "", fmt.Errorf("can't get last repo tag: %w", err)
	//}
	lastTag := "unknown"

	merges, err := getGitMerges(lastTag)
	if err != nil {
		return "", fmt.Errorf("can't get merges from last tag %q: %w", lastTag, err)
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
