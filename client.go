package pkggodev

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/PuerkitoBio/goquery"
	"github.com/gocolly/colly/v2"
)

type client struct {
	httpClient *http.Client
	baseURL    string
}

var ErrNotFound = errors.New("not found on pkg.go.dev")

type ErrorList struct {
	Errs []error
}

func (e *ErrorList) Error() string {
	return fmt.Sprintf("errors: %v", e.Errs)
}

func New(options ...func(c *client)) *client {
	c := &client{
		baseURL: "https://pkg.go.dev",
	}
	for _, opt := range options {
		opt(c)
	}
	return c
}

func WithBaseURL(url string) func(c *client) {
	return func(c *client) {
		c.baseURL = url
	}
}

func WithHTTPClient(httpClient *http.Client) func(c *client) {
	return func(c *client) {
		c.httpClient = httpClient
	}
}

func (c *client) newCollector() *colly.Collector {
	col := colly.NewCollector()
	if c.httpClient != nil {
		col.SetClient(c.httpClient)
	}
	return col
}

type ImportedByRequest struct {
	Package string
}

type ImportedBy struct {
	Package    string
	ImportedBy []string
}

func (c *client) ImportedBy(req ImportedByRequest) (*ImportedBy, error) {
	col := c.newCollector()
	importedBy := &ImportedBy{Package: req.Package}
	var err error

	col.OnHTML(".u-breakWord", func(e *colly.HTMLElement) {
		importedBy.ImportedBy = append(importedBy.ImportedBy, strings.TrimSpace(e.Text))
	})
	col.OnError(func(r *colly.Response, e error) {
		if r.StatusCode == 404 {
			err = ErrNotFound
			return
		}
		err = fmt.Errorf("making req to %s: %w", r.Request.URL.String(), e)
	})
	col.Visit(fmt.Sprintf("%s/%s?tab=importedby", c.baseURL, req.Package))
	if err != nil {
		return nil, err
	}
	return importedBy, nil
}

type DescribePackageRequest struct {
	Package string
}

type Image struct {
	Alt string
	URL string
}

type Package struct {
	Package                   string
	IsModule                  bool
	IsPackage                 bool
	Version                   string
	Published                 string
	License                   string
	HasValidGoModFile         bool
	HasRedistributableLicense bool
	HasTaggedVersion          bool
	HasStableVersion          bool
	Repository                string
	Synopsis                  string
	Images                    []Image
}

func (c *client) DescribePackage(req DescribePackageRequest) (*Package, error) {
	col := c.newCollector()
	p := &Package{Package: req.Package}
	errs := &ErrorList{}

	col.OnHTML("[data-test-id=UnitHeader-version]", func(e *colly.HTMLElement) {
		versionStr := e.DOM.Children().First().Text()
		version := strings.TrimSpace(strings.TrimPrefix(versionStr, "Version: "))
		p.Version = version
	})
	col.OnHTML("[data-test-id=UnitHeader-licenses]", func(e *colly.HTMLElement) {
		licenseStr := e.DOM.Children().First().Text()
		p.License = strings.TrimSpace(licenseStr)
	})
	col.OnHTML(".UnitMeta", func(e *colly.HTMLElement) {
		lis := e.DOM.Find("li")
		lis.Each(func(i int, s *goquery.Selection) {
			checked := s.Find("img[alt=checked]").Length() > 0
			switch i {
			case 0:
				p.HasValidGoModFile = checked
			case 1:
				p.HasRedistributableLicense = checked
			case 2:
				p.HasTaggedVersion = checked
			case 3:
				p.HasStableVersion = checked
			}
		})
	})
	col.OnHTML(".UnitMeta-repo", func(e *colly.HTMLElement) {
		text := e.DOM.Children().First().Text()
		p.Repository = strings.TrimSpace(strings.Trim(text, "\\n"))
	})
	col.OnHTML("[data-test-id=UnitHeader-commitTime]", func(e *colly.HTMLElement) {
		text := strings.TrimSpace(e.Text)
		dateStr := strings.TrimPrefix(text, "Published: ")
		t, err := normalizeTime(dateStr)
		if err != nil {
			errs.Errs = append(errs.Errs, err)
			return
		}
		p.Published = t
	})
	col.OnHTML(".UnitHeader-titleHeading", func(e *colly.HTMLElement) {
		for next := e.DOM.Next(); ; next = next.Next() {
			switch next.Text() {
			case "command":
				//pass
			case "package":
				p.IsPackage = true
			case "module":
				p.IsModule = true
			default:
				if !p.IsPackage && !p.IsModule {
					errs.Errs = append(errs.Errs, fmt.Errorf("IsPackage=false after parsing page for '%s', this probably indicates a parsing bug", req.Package))
				}
				return
			}
		}
	})
	col.OnHTML(".SearchSnippet-synopsis", func(e *colly.HTMLElement) {
		p.Synopsis = strings.TrimSpace(e.Text)
	})
	col.OnHTML(".UnitReadme-content img", func(e *colly.HTMLElement) {
		alt, _ := e.DOM.Attr("alt")
		src, _ := e.DOM.Attr("src")
		// Ensure URL is absolute
		url := src
		if !strings.HasPrefix(src, "http") {
			if strings.HasPrefix(src, "/") {
				url = c.baseURL + src
			} else {
				url = c.baseURL + "/" + src
			}
		}
		p.Images = append(p.Images, Image{
			Alt: alt,
			URL: url,
		})
	})

	col.OnError(func(r *colly.Response, e error) {
		if r.StatusCode == 404 {
			errs.Errs = append(errs.Errs, ErrNotFound)
			return
		}
		errs.Errs = append(errs.Errs, fmt.Errorf("making req to %s: %w", r.Request.URL.String(), e))
	})
	col.Visit(fmt.Sprintf("%s/%s", c.baseURL, req.Package))
	if len(errs.Errs) != 0 {
		return nil, errs
	}
	return p, nil
}

type Versions struct {
	Package  string
	Versions []Version
}

type Version struct {
	MajorVersion string
	FullVersion  string
	Date         string
}

type Change struct {
	URL            string
	Symbol         string
	SymbolSynopsis string
}

func normalizeTime(s string) (string, error) {
	var absTime time.Time

	if s == "today" {
		absTime = time.Now()
	} else if strings.Contains(s, "ago") {
		now := time.Now()
		split := strings.Split(s, " ")
		quantityStr := split[0]
		quantity, err := strconv.ParseInt(quantityStr, 10, 64)
		if err != nil {
			return "", fmt.Errorf("parsing quantity '%s' of time '%s': %w", quantityStr, s, err)
		}
		quantityDur := time.Duration(quantity)
		unit := strings.TrimSuffix(split[1], "s")

		switch unit {
		case "hour":
			absTime = now.Add(-quantityDur * time.Hour)
		case "day":
			absTime = now.AddDate(0, 0, -int(quantity))
		case "week":
			absTime = now.AddDate(0, 0, -7*int(quantity))
		default:
			return "", fmt.Errorf("unknown quantity '%s' when parsing '%s'", quantityStr, s)
		}
	} else {
		d, err := time.Parse("Jan 2, 2006", s)
		if err != nil {
			return "", fmt.Errorf("parsing date '%s': %w", s, err)
		}
		absTime = d
	}
	return absTime.Format("2006-01-02"), nil
}

type VersionsRequest struct {
	Package string
}

func (c *client) Versions(req VersionsRequest) (*Versions, error) {
	col := c.newCollector()
	errs := &ErrorList{}

	versions := &Versions{Package: req.Package}
	col.OnHTML(".Versions-list", func(e *colly.HTMLElement) {
		var curVersion Version
		var curMajorVersion string
		e.DOM.Children().Each(func(i int, s *goquery.Selection) {
			if s.HasClass("Version-major") {
				mv := strings.TrimSpace(s.Text())
				if mv != "" {
					curMajorVersion = mv
				}
				curVersion.MajorVersion = curMajorVersion
			}
			if s.HasClass("Version-tag") {
				version := s.Find(".js-versionLink").Text()
				curVersion.FullVersion = version
			}
			if s.HasClass("Version-commitTime") {
				dateStr := strings.TrimSpace(s.Text())
				t, err := normalizeTime(dateStr)
				if err != nil {
					errs.Errs = append(errs.Errs, err)
					return
				}
				curVersion.Date = t
				versions.Versions = append(versions.Versions, curVersion)
				curVersion = Version{}
			}
			if s.HasClass("Version-details") {
				s.Find(".Version-summary").Find("span").Remove()
				dateStr := strings.TrimSpace(s.Find(".Version-summary").Text())
				t, err := normalizeTime(dateStr)
				if err != nil {
					println("error in version details: " + err.Error())
					return
				}
				curVersion.Date = t
				versions.Versions = append(versions.Versions, curVersion)
				curVersion = Version{}
			}
		})
	})

	col.OnError(func(r *colly.Response, e error) {
		if r.StatusCode == 404 {
			errs.Errs = append(errs.Errs, ErrNotFound)
			return
		}
		errs.Errs = append(errs.Errs, fmt.Errorf("making req to %s: %w", r.Request.URL.String(), e))
	})

	col.Visit(fmt.Sprintf("%s/%s?tab=versions", c.baseURL, req.Package))
	if len(errs.Errs) > 0 {
		return nil, errs
	}
	return versions, nil
}

type SearchRequest struct {
	Query string
	Limit int
}

type SearchResults struct {
	Results []SearchResult
}

type SearchResult struct {
	Package    string
	Version    string
	Published  string
	ImportedBy int
	License    string
	Synopsis   string
}

func (c *client) Search(req SearchRequest) (*SearchResults, error) {
	col := c.newCollector()
	results := &SearchResults{}
	errs := &ErrorList{}

	morePages := true

	col.OnHTML("[data-test-id=results-total]", func(e *colly.HTMLElement) {
		resultsStr := strings.TrimSpace(e.Text)
		if resultsStr == "0 results" {
			return
		}
		resultsSplit := strings.Split(resultsStr, " ")
		if len(resultsSplit) == 2 {
			morePages = false
		} else {
			upperBoundStr := resultsSplit[2]
			upperBound, err := strconv.Atoi(upperBoundStr)
			if err != nil {
				errs.Errs = append(errs.Errs, err)
				return
			}

			totalResultsStr := resultsSplit[4]
			reachedReqLimit := upperBound >= req.Limit
			reachedLastPage := upperBoundStr == totalResultsStr
			morePages = !reachedReqLimit && !reachedLastPage
		}
	})

	col.OnHTML(".LegacySearchSnippet", func(e *colly.HTMLElement) {
		if len(results.Results) == req.Limit {
			return
		}

		pkg := strings.TrimSpace(e.DOM.Find("[data-test-id=snippet-title]").Text())
		synopsis := strings.TrimSpace(e.DOM.Find(".SearchSnippet-synopsis").Text())
		info := e.DOM.Find(".SearchSnippet-infoLabel")

		version := strings.TrimSpace(info.Find("[data-test-id=snippet-version]").Text())
		publishedDateStr := strings.TrimSpace(info.Find("[data-test-id=snippet-published]").Text())
		published, err := normalizeTime(publishedDateStr)
		if err != nil {
			errs.Errs = append(errs.Errs, err)
			return
		}
		importedByWithCommas := strings.TrimSpace(info.Find("[data-test-id=snippet-importedby]").Text())
		importedByStr := strings.ReplaceAll(importedByWithCommas, ",", "")
		importedBy, err := strconv.Atoi(importedByStr)
		if err != nil {
			errs.Errs = append(errs.Errs, err)
			return
		}
		license := strings.TrimSpace(info.Find("[data-test-id=snippet-license]").Text())
		result := SearchResult{
			Package:    pkg,
			Synopsis:   synopsis,
			Version:    version,
			Published:  published,
			ImportedBy: importedBy,
			License:    license,
		}
		results.Results = append(results.Results, result)
	})
	col.OnError(func(r *colly.Response, e error) {
		errs.Errs = append(errs.Errs, e)
	})
	for page := 1; morePages; page++ {
		col.Visit(fmt.Sprintf("%s/search?q=%s&m=package&page=%d", c.baseURL, req.Query, page))
		if len(errs.Errs) > 0 {
			return nil, errs
		}
	}

	return results, nil
}

type ImportsRequest struct {
	Package string
}

type Imports struct {
	Package                string
	Imports                []string
	ModuleImports          map[string][]string
	StandardLibraryImports []string
}

func (c *client) Imports(req ImportsRequest) (*Imports, error) {
	return nil, nil
}

type LicensesRequest struct {
	Package string
}

type License struct {
	Name     string
	Source   string
	FullText string
}

func (c *client) Licenses(req LicensesRequest) ([]License, error) {
	return nil, nil
}
