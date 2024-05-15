package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"image/color"
	"io"
	"log"
	"math"
	"mime/multipart"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"go-hep.org/x/hep/hplot"

	"github.com/dustin/go-humanize"
	_ "github.com/lib/pq"
	"github.com/nbd-wtf/go-nostr"
	"github.com/nbd-wtf/go-nostr/nip19"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"gonum.org/v1/plot"
	"gonum.org/v1/plot/plotter"
	"gonum.org/v1/plot/vg"
	"gonum.org/v1/plot/vg/draw"
)

const name = "nostr-btcchart"

const version = "0.0.14"

var revision = "HEAD"

type BtcLog struct {
	bun.BaseModel `bun:"table:btclog,alias:f"`
	Timestamp     int64     `bun:"timestamp,pk,notnull" json:"timestamp"`
	Last          float64   `bun:"last,notnull" json:"last"`
	Bid           float64   `bun:"bid,notnull" json:"bid"`
	Ask           float64   `bun:"ask,notnull" json:"ask"`
	CreatedAt     time.Time `bun:"created_at,notnull,default:current_timestamp" json:"created_at"`
}

type XTicks struct {
	Ticker plot.Ticker
	Time   func(t float64) time.Time
}

func (t XTicks) Ticks(min, max float64) []plot.Tick {
	ticks := []plot.Tick{}
	tmcur := time.Unix(int64(min), 0)
	tmmax := time.Unix(int64(max), 0)
	if max-min < 15000 {
		tmcur = time.Date(tmcur.Year(), tmcur.Month(), tmcur.Day(), tmcur.Hour(), tmcur.Minute()-tmcur.Minute()%10, 0, 0, tmcur.Location())
		tmmax = time.Date(tmmax.Year(), tmmax.Month(), tmmax.Day(), tmmax.Hour(), tmmax.Minute()-tmmax.Minute()%10, 0, 0, tmmax.Location())
	} else if max-min < 90000 {
		tmcur = time.Date(tmcur.Year(), tmcur.Month(), tmcur.Day(), tmcur.Hour(), 0, 0, 0, tmcur.Location())
		tmmax = time.Date(tmmax.Year(), tmmax.Month(), tmmax.Day(), tmmax.Hour(), 0, 0, 0, tmmax.Location())
	} else {
		tmcur = time.Date(tmcur.Year(), tmcur.Month(), tmcur.Day(), 0, 0, 0, 0, tmcur.Location())
		tmmax = time.Date(tmmax.Year(), tmmax.Month(), tmmax.Day(), 0, 0, 0, 0, tmmax.Location())
	}
	c := 0
	for {
		tick := plot.Tick{Value: float64(tmcur.Unix())}
		switch delta := max - min; {
		case delta < 864000:
			// delta is less than 10 days
			// - mayor: every day (min: 0, max: 10)
			// - minor: every day (min: 0, max: 10)
			if delta < 90000 {
				tick.Label = tmcur.Format("15:04")
			} else {
				tick.Label = tmcur.Format("01/02")
			}
			ticks = append(ticks, tick)
		case delta < 7776000:
			// delta is between 10 and 90 days
			// - mayor: every 5 days (min: 2, max: 18)
			// - minor: every day (min: 10, max: 90)
			if c%5 == 0 {
				tick.Label = tmcur.Format("01/02")
			}
			ticks = append(ticks, tick)
		case delta < 15552000:
			// delta is between 90 and 180 days
			// mayor: on day 1 and 15 of every month (min: 5, max: 12)
			// minor: on day 1, 5, 10, 15, 20, 25, 30 of every month (min: 17, max: 36)
			if tmcur.Day() == 1 || tmcur.Day() == 15 {
				tick.Label = tmcur.Format("01/02")
			}
			if tmcur.Day() == 1 || tmcur.Day()%5 == 0 {
				ticks = append(ticks, tick)
			}
		case delta < 47347200:
			// delta is between 6 months and 18 months
			// mayor: on day 1 of every month (min: 5, max: 18)
			// minor: on day 1 and 15 of every month (min: 11, max: 36)
			if tmcur.Day() == 1 {
				tick.Label = tmcur.Format("2004/01")
			}
			if tmcur.Day() == 1 || tmcur.Day() == 15 {
				ticks = append(ticks, tick)
			}
		default:
			// delta is higher than 18 months
			// mayor: on the 1st of january (min: 1, max: inf.)
			// minor: on day 1 of every month (min: 17, max inf.)
			if tmcur.Day() == 1 && tmcur.Month() == time.January {
				tick.Label = tmcur.Format("2004/01")
			}
			if tmcur.Day() == 1 {
				ticks = append(ticks, tick)
			}
		}
		c = c + 1
		if max-min < 15000 {
			tmcur = tmcur.Add(10 * time.Minute)
		} else if max-min < 87000 {
			tmcur = tmcur.Add(1 * time.Hour)
		} else {
			tmcur = tmcur.AddDate(0, 0, 1)
		}
		if tmcur.After(tmmax) {
			break
		}
	}
	return ticks
}

func upload(buf *bytes.Buffer) (string, error) {
	var b bytes.Buffer
	w := multipart.NewWriter(&b)
	part, err := w.CreateFormFile("fileToUpload", "fileToUpload")
	if err != nil {
		log.Fatalf("CreateFormFile: %v", err)
	}
	part.Write(buf.Bytes())
	err = w.Close()
	if err != nil {
		log.Fatalf("Close: %v", err)
	}
	req, err := http.NewRequest(http.MethodPost, "https://nostr.build/api/upload/ios.php", &b)
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", w.FormDataContentType())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer req.Body.Close()

	if resp.StatusCode != 200 {
		if b, err := io.ReadAll(resp.Body); err == nil {
			return "", errors.New(string(b))
		}
	}

	var p string
	err = json.NewDecoder(resp.Body).Decode(&p)
	if err != nil {
		return "", err
	}
	return p, nil
}

func generate(bundb *bun.DB, span int, output string) (string, error) {
	if span < 2 || span > 43200 {
		return "", errors.New("invalid request")
	}
	var data []BtcLog
	err := bundb.NewSelect().Model((*BtcLog)(nil)).Order("timestamp DESC").Limit(span).Scan(context.Background(), &data)
	if err != nil {
		return "", err
	}

	sort.Slice(data, func(i, j int) bool {
		return data[i].Timestamp < data[j].Timestamp
	})

	var points plotter.XYs
	for _, d := range data {
		points = append(points, plotter.XY{
			X: float64(d.Timestamp),
			Y: d.Ask,
		})
	}

	p := plot.New()
	p.Title.TextStyle.Color = color.White
	p.BackgroundColor = color.Black
	p.Title.Text = fmt.Sprintf("₿ ¥ %s", humanize.Comma(int64(points[len(points)-1].Y)))
	p.Add(plotter.NewGrid())

	//p.X.Label.Text = "Time"
	p.X.Color = color.White
	p.X.Label.TextStyle.Color = color.White
	p.X.Label.Padding = vg.Points(10)
	p.X.LineStyle.Color = color.White
	p.X.LineStyle.Width = vg.Points(1)
	p.X.Tick.Color = color.White
	p.X.Tick.Marker = XTicks{}
	p.X.Tick.Label.Rotation = math.Pi / 3
	p.X.Tick.Label.XAlign = -1.2
	p.X.Tick.Label.Color = color.White

	//p.Y.Label.Text = "JPY/BTC"
	p.Y.Color = color.White
	p.Y.Label.TextStyle.Color = color.White
	p.Y.LineStyle.Color = color.White
	p.Y.LineStyle.Width = vg.Points(1)
	p.Y.Tick.Color = color.White
	p.Y.Tick.Label.Color = color.White
	p.Y.Tick.Marker = hplot.Ticks{
		N:      10,
		Format: "%.0f",
	}
	p.Y.Tick.Label.Color = color.White
	p.Y.Label.Position = draw.PosRight
	p.X.Label.Position = draw.PosTop

	line, err := plotter.NewLine(points)
	if err != nil {
		log.Println(err)
	}
	line.Color = color.RGBA{R: 50, G: 255, B: 100, A: 255}
	p.Add(line)

	if output != "" {
		err := p.Save(5*vg.Inch, 4*vg.Inch, output)
		return "", err
	}
	var buf bytes.Buffer
	w, err := p.WriterTo(5*vg.Inch, 4*vg.Inch, "png")
	if err != nil {
		return "", err
	}
	_, err = w.WriteTo(&buf)
	if err != nil {
		return "", err
	}

	return upload(&buf)
}

func handler(bundb *bun.DB, nsec string) func(w http.ResponseWriter, r *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			w.Header().Set("content-type", "text/plain; charset=utf-8")
			var data []BtcLog
			err := bundb.NewSelect().Model((*BtcLog)(nil)).Order("timestamp DESC").Limit(180).Scan(context.Background(), &data)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			json.NewEncoder(w).Encode(data)
			return
		}
		var ev nostr.Event
		err := json.NewDecoder(r.Body).Decode(&ev)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		tok := strings.Split(ev.Content, " ")
		span := 180 * time.Minute
		if len(tok) == 2 {
			span, err = time.ParseDuration(tok[1])
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
		}

		img, err := generate(bundb, int(span/time.Minute), "")
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		eev := nostr.Event{}
		var sk string
		if _, s, err := nip19.Decode(nsec); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		} else {
			sk = s.(string)
		}
		if pub, err := nostr.GetPublicKey(sk); err == nil {
			if _, err := nip19.EncodePublicKey(pub); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
			}
			eev.PubKey = pub
		} else {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}

		eev.Content = img
		eev.CreatedAt = nostr.Now()
		eev.Kind = ev.Kind
		eev.Tags = eev.Tags.AppendUnique(nostr.Tag{"e", ev.ID, "", "reply"})
		for _, te := range ev.Tags {
			if te.Key() == "e" {
				eev.Tags = eev.Tags.AppendUnique(te)
			}
		}
		eev.Sign(sk)

		w.Header().Set("content-type", "text/json; charset=utf-8")
		json.NewEncoder(w).Encode(eev)
	}
}

func init() {
}

func main() {
	var dsn string
	var ver bool
	var span time.Duration
	var output string

	flag.StringVar(&dsn, "dsn", os.Getenv("DATABASE_URL"), "Database source")
	flag.DurationVar(&span, "span", 180*time.Minute, "span")
	flag.StringVar(&output, "output", "", "output filename")
	flag.BoolVar(&ver, "v", false, "show version")
	flag.Parse()

	if ver {
		fmt.Println(version)
		os.Exit(0)
	}

	time.Local = time.FixedZone("Local", 9*60*60)

	db, err := sql.Open("postgres", dsn)
	if err != nil {
		log.Fatal(err)
	}

	bundb := bun.NewDB(db, pgdialect.New())
	defer bundb.Close()

	if output != "" {
		_, err := generate(bundb, int(span/time.Minute), output)
		if err != nil {
			log.Fatal(err)
		}
		return
	}

	nsec := os.Getenv("NULLPOGA_NSEC")
	if nsec == "" {
		log.Fatal("NULLPOGA_NSEC is not set")
	}

	http.HandleFunc("/", handler(bundb, nsec))
	addr := ":" + os.Getenv("PORT")
	if addr == ":" {
		addr = ":8080"
	}
	log.Printf("started %v", addr)
	http.ListenAndServe(addr, nil)
}
