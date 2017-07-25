package main

import (
	"errors"
	"flag"
	"fmt"
	"net/http"
	_ "net/http/pprof"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	tc "github.com/guidewire/teamcity-go-bindings"
	"github.com/orcaman/concurrent-map"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/log"
	"github.com/prometheus/common/version"
)

const (
	namespace = "teamcity"
)

var metricsStorage = cmap.New()

var (
	instanceStatus = prometheus.NewDesc(
		prometheus.BuildFQName(namespace, "", "instance_status"),
		"Teamcity instance status",
		[]string{"instance"}, nil,
	)
	instanceLastScrapeFinishTime = prometheus.NewDesc(
		prometheus.BuildFQName(namespace, "", "instance_last_scrape_finish_time"),
		"Teamcity instance last scrape finish time",
		[]string{"instance"}, nil,
	)
	instanceLastScrapeDuration = prometheus.NewDesc(
		prometheus.BuildFQName(namespace, "", "instance_last_scrape_duration"),
		"Teamcity instance last scrape duration",
		[]string{"instance"}, nil,
	)
)

func init() {
	runtime.GOMAXPROCS(runtime.NumCPU())
	prometheus.MustRegister(version.NewCollector("teamcity_exporter"))
}

func main() {
	var (
		showVersion   = flag.Bool("version", false, "Print version information")
		listenAddress = flag.String("web.listen-address", ":9107", "Address to listen on for web interface and telemetry")
		metricsPath   = flag.String("web.telemetry-path", "/metrics", "Path under which to expose metrics")
		configPath    = flag.String("config", "config.yaml", "Path to configuration file")
	)
	flag.Parse()

	log.Infoln("Starting teamcity_exporter", version.Info())
	log.Infoln("Build context", version.BuildContext())

	if *showVersion {
		log.Infoln(os.Stdout, version.Print("teamcity_exporter"))
		return
	}

	collector := NewCollector()
	prometheus.MustRegister(collector)

	config := Configuration{}
	if err := config.parseConfig(*configPath); err != nil {
		log.Fatalf("Failed to parse configuration file: %v", err)
	}
	if err := config.validateConfig(); err != nil {
		log.Fatalf("Failed to validate configuration: %v", err)
	}

	for i := range config.Instances {
		go config.Instances[i].collectStat()
	}

	http.Handle(*metricsPath, prometheus.Handler())
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`<html>
					 <head><title>Teamcity Exporter</title></head>
					 <body>
					 <h1>Teamcity Exporter</h1>
					 <p><a href='` + *metricsPath + `'>Metrics</a></p>
					 </body>
					 </html>`))
	})
	log.Infoln("Listening on", *listenAddress)
	log.Fatalln(http.ListenAndServe(*listenAddress, nil))
}

func (i *Instance) collectStat() {
	ticker := newTicker(time.Duration(i.ScrapeInterval) * time.Second)
	for _ = range ticker.c {
		go i.collectStatHandler()
	}
}

func (i *Instance) collectStatHandler() {
	startProcessing := time.Now()
	client := tc.New(i.URL, i.Username, i.Password)

	chBuildFilter := make(chan BuildFilter)
	chBuild := make(chan Build)
	chBuildStat := make(chan BuildStatistics)

	wg := &sync.WaitGroup{}
	wg.Add(4)
	go i.prepareFilters(client, wg, chBuildFilter)
	go getBuildsByFilters(client, wg, chBuildFilter, chBuild)
	go getBuildStat(client, wg, chBuild, chBuildStat)
	go parseStat(wg, chBuildStat)

	wg.Wait()
	finishProcessing := time.Now()
	metricsStorage.Set(getHash(instanceLastScrapeFinishTime.String(), i.Name), prometheus.MustNewConstMetric(instanceLastScrapeFinishTime, prometheus.GaugeValue, float64(finishProcessing.Unix()), i.Name))
	metricsStorage.Set(getHash(instanceLastScrapeDuration.String(), i.Name), prometheus.MustNewConstMetric(instanceLastScrapeDuration, prometheus.GaugeValue, float64(finishProcessing.Sub(startProcessing)/time.Second), i.Name))
}

func (i *Instance) prepareFilters(c *tc.Client, wg *sync.WaitGroup, ch chan<- BuildFilter) {
	defer wg.Done()

	if len(i.BuildsFilters) == 0 {
		i.addDefaultFilter()
	}

	for k := range i.BuildsFilters {
		bt := tc.BuildConfiguration{}
		b := map[tc.BuildTypeID][]tc.Branch{}

		if i.BuildsFilters[k].Filter.BuildType == "" {
			var err error
			bt, err = c.GetAllBuildConfigurations()
			if err != nil {
				log.Errorf("Failed to query available build configurations for instance '%s': %v", i.Name, err)
			}
			continue
		} else {
			bt = tc.BuildConfiguration{BuildTypes: []tc.BuildType{{ID: tc.BuildTypeID(i.BuildsFilters[k].Filter.BuildType)}}}
		}

		if i.BuildsFilters[k].Filter.Branch == "" {
			for v := range bt.BuildTypes {
				branches, err := c.GetAllBranches(bt.BuildTypes[v].ID)
				if err != nil {
					log.Errorf("Failed to query branches for '%s' build configuration: %v", bt.BuildTypes[v].ID, err)
					continue
				}

				// only default branch exists, can't use it in filter because teamcity will ignore it
				if len(branches.Branch) == 1 {
					b[bt.BuildTypes[v].ID] = []tc.Branch{}
				} else {
					b[bt.BuildTypes[v].ID] = branches.Branch
				}
			}
		} else {
			for v := range bt.BuildTypes {
				b[bt.BuildTypes[v].ID] = []tc.Branch{{Name: i.BuildsFilters[k].Filter.Branch}}
			}
		}

		for bt, branches := range b {
			if len(branches) == 0 {
				f := BuildFilter{
					Name:     i.BuildsFilters[k].Name,
					instance: i.Name,
					Filter: tc.BuildLocator{
						BuildType: string(bt),
						Branch:    "",
						Count:     "1"},
				}
				ch <- f
			} else {
				for z := range branches {
					f := BuildFilter{
						Name:     i.BuildsFilters[k].Name,
						instance: i.Name,
						Filter: tc.BuildLocator{
							BuildType: string(bt),
							Branch:    branches[z].Name,
							Count:     "1"},
					}
					ch <- f
				}
			}

		}
	}
	close(ch)
}

func getBuildsByFilters(c *tc.Client, wg *sync.WaitGroup, chIn <-chan BuildFilter, chOut chan<- Build) {
	defer wg.Done()
	wg1 := &sync.WaitGroup{}

	for i := range chIn {
		wg1.Add(1)
		go func(i BuildFilter) {
			defer wg1.Done()
			b, err := c.GetBuildsByParams(i.Filter)
			if err != nil {
				log.Errorf("Failed to query builds by filter '%s': %v", i.Name, err)
				return
			}
			for v := range b.Build {
				chOut <- Build{Details: b.Build[v], Filter: i}
			}
		}(i)
	}

	wg1.Wait()
	close(chOut)
}

func getBuildStat(c *tc.Client, wg *sync.WaitGroup, chIn <-chan Build, chOut chan<- BuildStatistics) {
	defer wg.Done()
	wg1 := &sync.WaitGroup{}
	for i := range chIn {
		wg1.Add(1)
		go func(i Build) {
			defer wg1.Done()
			s, err := c.GetBuildStat(int(i.Details.ID))
			if err != nil {
				log.Errorf("Failed to query build statistics for build %s: %v", i.Details.WebURL, err)
				return
			}
			chOut <- BuildStatistics{Build: i, Stat: s}
		}(i)
	}

	wg1.Wait()
	close(chOut)
}

func parseStat(wg *sync.WaitGroup, chIn <-chan BuildStatistics) {
	defer wg.Done()

	for i := range chIn {
		for k := range i.Stat.Property {
			value, err := strconv.ParseFloat(i.Stat.Property[k].Value, 64)
			if err != nil {
				log.Errorf("Failed to convert string '%s' to float: %v", i.Stat.Property[k].Value, err)
				continue
			}
			metric := strings.SplitN(i.Stat.Property[k].Name, ":", 2)
			title := fmt.Sprint(namespace, "_", toSnakeCase(metric[0]))

			labels := []Label{
				{"exporter_instance", i.Build.Filter.instance},
				{"exporter_filter", i.Build.Filter.Name},
				{"build_configuration", string(i.Build.Details.BuildTypeID)},
				{"branch", i.Build.Details.BranchName},
			}
			if len(metric) > 1 {
				labels = append(labels, Label{"other", metric[1]})
			}

			labelsTitles, labelsValues := []string{}, []string{}
			for v := range labels {
				labelsTitles = append(labelsTitles, labels[v].Name)
				labelsValues = append(labelsValues, labels[v].Value)
			}

			desc := prometheus.NewDesc(title, title, labelsTitles, nil)
			metricsStorage.Set(getHash(title, labelsValues...), prometheus.MustNewConstMetric(desc, prometheus.GaugeValue, value, labelsValues...))
		}
	}
}

func (i *Instance) validateStatus() error {
	client := &http.Client{}
	req, err := http.NewRequest("GET", i.URL, nil)
	if err != nil {
		metricsStorage.Set(getHash(instanceStatus.String(), i.Name), prometheus.MustNewConstMetric(instanceStatus, prometheus.GaugeValue, 0, i.Name))
		return err
	}
	req.SetBasicAuth(i.Username, i.Password)
	resp, err := client.Do(req)
	if err != nil {
		metricsStorage.Set(getHash(instanceStatus.String(), i.Name), prometheus.MustNewConstMetric(instanceStatus, prometheus.GaugeValue, 0, i.Name))
		return err
	}
	if resp.StatusCode == 401 {
		metricsStorage.Set(getHash(instanceStatus.String(), i.Name), prometheus.MustNewConstMetric(instanceStatus, prometheus.GaugeValue, 0, i.Name))
		return errors.New("Unauthorized instance " + i.Name)
	}
	metricsStorage.Set(getHash(instanceStatus.String(), i.Name), prometheus.MustNewConstMetric(instanceStatus, prometheus.GaugeValue, 1, i.Name))
	return nil
}

func (i *Instance) addDefaultFilter() BuildFilter {
	f := BuildFilter{
		Name:     "default",
		instance: i.Name,
		Filter:   *tc.NewBuildLocator(),
	}
	i.BuildsFilters = append(i.BuildsFilters, f)
	return f
}
