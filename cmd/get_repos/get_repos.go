package main

import (
	"fmt"
	"io/ioutil"
	"os"
	"sort"
	"strings"
	"time"

	lib "devstats"

	yaml "gopkg.in/yaml.v2"
)

// dirExists checks if given path exist and if is a directory
func dirExists(path string) (bool, error) {
	if path[len(path)-1:] == "/" {
		path = path[:len(path)-1]
	}
	stat, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	if stat.IsDir() {
		return true, nil
	}
	return false, fmt.Errorf("%s: exists, but is not a directory", path)
}

// getRepos returns map { 'org' --> list of repos } for all devstats projects
func getRepos(ctx *lib.Ctx) (map[string]bool, map[string][]string) {
	// Local or cron mode?
	dataPrefix := lib.DataDir
	if ctx.Local {
		dataPrefix = "./"
	}

	// Read defined projects
	data, err := ioutil.ReadFile(dataPrefix + "projects.yaml")
	lib.FatalOnError(err)

	var projects lib.AllProjects
	lib.FatalOnError(yaml.Unmarshal(data, &projects))
	dbs := make(map[string]bool)
	for _, proj := range projects.Projects {
		if proj.Disabled {
			continue
		}
		dbs[proj.PDB] = true
	}

	allRepos := make(map[string][]string)
	for db := range dbs {
		// Connect to Postgres `db` database.
		con := lib.PgConnDB(ctx, db)
		defer con.Close()

		// Get list of orgs in a given database
		rows, err := con.Query("select distinct name from gha_repos where name like '%/%'")
		lib.FatalOnError(err)
		defer rows.Close()
		var (
			repo  string
			repos []string
		)
		for rows.Next() {
			lib.FatalOnError(rows.Scan(&repo))
			repos = append(repos, repo)
		}
		lib.FatalOnError(rows.Err())

		// Create map of distinct "org" --> list of repos
		for _, repo := range repos {
			ary := strings.Split(repo, "/")
			if len(ary) != 2 {
				lib.FatalOnError(fmt.Errorf("invalid repo name: %s", repo))
			}
			org := ary[0]
			_, ok := allRepos[org]
			if !ok {
				allRepos[org] = []string{}
			}
			ary = append(allRepos[org], repo)
			allRepos[org] = ary
		}
	}

	// return final map
	return dbs, allRepos
}

// processRepo - processes single repo (clone or reset+pull) in a separate thread/goroutine
func processRepo(ch chan string, ctx *lib.Ctx, orgRepo, rwd string) {
	exists, err := dirExists(rwd)
	lib.FatalOnError(err)
	if !exists {
		// We need to clone repo
		if ctx.Debug > 0 {
			lib.Printf("Cloning %s\n", orgRepo)
		}
		dtStart := time.Now()
		res := lib.ExecCommand(
			ctx,
			[]string{"git", "clone", "https://github.com/" + orgRepo + ".git", rwd},
			map[string]string{"GIT_TERMINAL_PROMPT": "0"},
		)
		dtEnd := time.Now()
		if res != nil {
			if ctx.Debug > 0 {
				lib.Printf("Warining git-clone failed: %s (took %v): %+v\n", orgRepo, dtEnd.Sub(dtStart), res)
			}
			fmt.Fprintf(os.Stderr, "Warining git-clone failed: %s (took %v): %+v\n", orgRepo, dtEnd.Sub(dtStart), res)
			ch <- ""
			return
		}
		if ctx.Debug > 0 {
			lib.Printf("Cloned %s: took %v\n", orgRepo, dtEnd.Sub(dtStart))
		}
	} else {
		// We *may* need to pull repo
		if ctx.Debug > 0 {
			lib.Printf("Pulling %s\n", orgRepo)
		}
		dtStart := time.Now()
		res := lib.ExecCommand(
			ctx,
			[]string{"git_reset_pull.sh", rwd},
			map[string]string{"GIT_TERMINAL_PROMPT": "0"},
		)
		dtEnd := time.Now()
		if res != nil {
			if ctx.Debug > 0 {
				lib.Printf("Warining git-reset failed: %s (took %v): %+v\n", orgRepo, dtEnd.Sub(dtStart), res)
			}
			fmt.Fprintf(os.Stderr, "Warining git-reset failed: %s (took %v): %+v\n", orgRepo, dtEnd.Sub(dtStart), res)
			ch <- ""
			return
		}
		if ctx.Debug > 0 {
			lib.Printf("Pulled %s: took %v\n", orgRepo, dtEnd.Sub(dtStart))
		}
	}
	ch <- orgRepo
}

// processRepos process map of org -> list of repos to clone or pull them as needed
// it also displays cncf/gitdm needed info in debug mode (called manually)
func processRepos(ctx *lib.Ctx, allRepos map[string][]string) {
	// Set non-fatal exec mode, we want to run sync for next project(s) if current fails
	// Also set quite mode, many git-pulls or git-clones can fail and this is not needed to log it to DB
	// User can set higher debug level and run manually to debug this
	ctx.ExecFatal = false
	ctx.ExecQuiet = true

	// Go to main repos directory
	wd := ctx.ReposDir
	exists, err := dirExists(wd)
	lib.FatalOnError(err)
	if !exists {
		// Try to Mkdir it if not exists
		lib.FatalOnError(os.Mkdir(wd, 0755))
		exists, err = dirExists(wd)
		lib.FatalOnError(err)
		if !exists {
			lib.FatalOnError(fmt.Errorf("failed to create directory: %s", wd))
		}
	}

	// Process all orgs & repos
	thrN := lib.GetThreadsNum(ctx)
	chanPool := []chan string{}
	allOkRepos := []string{}
	checked := 0
	// Iterate orgs
	for org, repos := range allRepos {
		// Go to current 'org' subdirectory
		owd := wd + org
		exists, err = dirExists(owd)
		lib.FatalOnError(err)
		if !exists {
			// Try to Mkdir it if not exists
			lib.FatalOnError(os.Mkdir(owd, 0755))
			exists, err = dirExists(owd)
			lib.FatalOnError(err)
			if !exists {
				lib.FatalOnError(fmt.Errorf("failed to create directory: %s", owd))
			}
		}
		// Iterate org's repositories
		for _, orgRepo := range repos {
			ch := make(chan string)
			chanPool = append(chanPool, ch)
			// repository's working dir (if present we only need to do git reset --hard; git pull)
			ary := strings.Split(orgRepo, "/")
			repo := ary[1]
			rwd := owd + "/" + repo
			go processRepo(ch, ctx, orgRepo, rwd)
			checked++
			if len(chanPool) == thrN {
				ch = chanPool[0]
				res := <-ch
				chanPool = chanPool[1:]
				if res != "" {
					allOkRepos = append(allOkRepos, res)
				}
			}
		}
	}
	for _, ch := range chanPool {
		res := <-ch
		if res != "" {
			allOkRepos = append(allOkRepos, res)
		}
	}

	// Output all repos as ruby object & Final cncf/gitdm command to generate concatenated git.log
	// Only output when GHA2DB_EXTERNAL_INFO env variable is set
	// Only output to stdout - not standard logs via lib.Printf(...)
	if ctx.ExternalInfo {
		// Sort list of repos
		sort.Strings(allOkRepos)

		// Create Ruby-like string with all repos array
		allOkReposStr := "[\n"
		for _, okRepo := range allOkRepos {
			allOkReposStr += "  '" + okRepo + "',\n"
		}
		allOkReposStr += "]"

		// Create list of orgs
		orgs := []string{}
		for org := range allRepos {
			orgs = append(orgs, org)
		}

		// Sort orgs
		sort.Strings(orgs)

		// Output shell command sorted
		finalCmd := "./all_repos_log.sh "
		for _, org := range orgs {
			finalCmd += ctx.ReposDir + org + "/* "
		}

		// Output cncf/gitdm related data
		fmt.Printf("AllRepos:\n%s\n", allOkReposStr)
		fmt.Printf("Final command:\n%s\n", finalCmd)
	}
	lib.Printf("Sucesfully processed %d/%d repos\n", len(allOkRepos), checked)
}

// processCommitsDB creates/updates mapping between commits and list of files they refer to on databse 'db'
// using 'query' to get liist of unprocessed commits
func processCommitsDB(ch chan bool, ctx *lib.Ctx, db, query string) {
	// Conditional info
	if ctx.Debug > 0 {
		lib.Printf("Running on database: %s\n", db)
	}

	// Close channel on end no matter what happens
	defer func() {
		ch <- true
	}()

	// Get list of unprocessed commits for current DB
	dtStart := time.Now()
	// Connect to Postgres `db` database.
	con := lib.PgConnDB(ctx, db)
	defer con.Close()

	rows, err := con.Query(query)
	lib.FatalOnError(err)
	defer rows.Close()
	var (
		sha  string
		repo string
		shas [][2]string
	)
	for rows.Next() {
		lib.FatalOnError(rows.Scan(&sha, &repo))
		shas = append(shas, [2]string{repo, sha})
	}
	lib.FatalOnError(rows.Err())
	dtEnd := time.Now()
	if ctx.Debug > 0 {
		lib.Printf("Database '%s' processed in %v, commits: %d\n", db, dtEnd.Sub(dtStart), len(shas))
	}
	for i, data := range shas {
		repo := data[0]
		sha := data[1]
		fmt.Printf("Processing commit %06d %s:%s:%s\n", i, db, repo, sha)
		// TODO: continue here: get list of files affected by commit 'sha' on 'repo' repository
		// And put results into db:gha_commits_files table.
		// Algorithm consideration:
		// Create map of 'repo' --> list of commits from this repo
		// cd to cloned repo (it is cloned or pulled to most recent state by this tool)
		// git log for list of commits to get affected files
		// group by repo to avoid multiple chdirs and
		// possibly call single git log for multiple commits (rather not?)
	}
}

// processCommits process all databases given in `dbs`
// on each database it creates/updates mapping between commits and list of files they refer to
// It is multithreaded processing up to NCPU databases at the same time
func processCommits(ctx *lib.Ctx, dbs map[string]bool) {
	// Read SQL to get commits to sync from 'util_sql/list_unprocessed_commits.sql' file.
	// Local or cron mode?
	dataPrefix := lib.DataDir
	if ctx.Local {
		dataPrefix = "./"
	}
	bytes, err := ioutil.ReadFile(
		dataPrefix + "util_sql/list_unprocessed_commits.sql",
	)
	lib.FatalOnError(err)
	sqlQuery := string(bytes)

	// Process all DBs in a separate threads
	thrN := lib.GetThreadsNum(ctx)
	chanPool := []chan bool{}
	for db := range dbs {
		ch := make(chan bool)
		chanPool = append(chanPool, ch)
		go processCommitsDB(ch, ctx, db, sqlQuery)
		if len(chanPool) == thrN {
			ch = chanPool[0]
			<-ch
			chanPool = chanPool[1:]
		}
	}
	for _, ch := range chanPool {
		<-ch
	}
}

func main() {
	dtStart := time.Now()
	// Environment context parse
	var ctx lib.Ctx
	ctx.Init()
	dbs, repos := getRepos(&ctx)
	if ctx.ProcessRepos {
		processRepos(&ctx, repos)
	}
	if ctx.ProcessCommits {
		processCommits(&ctx, dbs)
	}
	dtEnd := time.Now()
	lib.Printf("All repos processed in: %v\n", dtEnd.Sub(dtStart))
}