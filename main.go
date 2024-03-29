package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"path/filepath"
	"reflect"
	"strconv"

	"cloud.google.com/go/storage"
	"github.com/olivere/elastic"
	"github.com/pborman/uuid"
)

const (
	POST_INDEX = "post"
	DISTANCE   = "200km"

	ES_URL      = "http://10.128.0.2:9200"
	BUCKET_NAME = "around-bucket-go-project"
)

var (
	mediaTypes = map[string]string{
		".jpeg": "image",
		".jpg":  "image",
		".gif":  "image",
		".png":  "image",
		".mov":  "video",
		".mp4":  "video",
		".avi":  "video",
		".flv":  "video",
		".wmv":  "video",
	}
)

type Location struct {
	Lat float64 `json:"lat"`
	Lon float64 `json:"lon"`
}

type Post struct {
	// `json:"user"` is for the json parsing of this User field.
	// Otherwise, by default it's 'User'.
	User     string   `json:"user"`
	Message  string   `json:"message"`
	Location Location `json:"location"`
	Url      string   `json:"url"`
	Type     string   `json:"type"`
	Face     float32  `json:"face"`
}

func main() {
	fmt.Println("started-service")
	http.HandleFunc("/post", handlerPost)
	http.HandleFunc("/search", handlerSearch)
	http.HandleFunc("/cluster", handlerCluster)
	// Fatal = print output and exit
	log.Fatal(http.ListenAndServe(":8080", nil))

}

// Refer to "JSON and Go"
// If a user sends a HTTP request with a body as
// {
//   “user”: “jack”
//   “message”: “this is a message”
// }
// Then it will automatically construct a Post object named p
// and update its values to be p.User = “jack”
// and p.Message = “this  is a message”
func handlerPost(w http.ResponseWriter, r *http.Request) {
	// Parse from body of request to get a json object.
	// fmt.Println("Received one post request")
	// decoder := json.NewDecoder(r.Body)
	// var p Post
	// if err := decoder.Decode(&p); err != nil {
	// 	panic(err)
	// }

	// fmt.Fprintf(w, "Post received: %s\n", p.Message)

	fmt.Println("Received one post request")
	w.Header().Set("Content-Type", "application/json")

	lat, _ := strconv.ParseFloat(r.FormValue("lat"), 64)
	lon, _ := strconv.ParseFloat(r.FormValue("lon"), 64)

	p := &Post{
		User:    r.FormValue("user"),
		Message: r.FormValue("message"),
		Location: Location{
			Lat: lat,
			Lon: lon,
		},
	}

	file, header, err := r.FormFile("image")
	if err != nil {
		http.Error(w, "Image is not available", http.StatusBadRequest)
		fmt.Printf("Image is not available %v\n", err)
		return
	}

	suffix := filepath.Ext(header.Filename)
	if t, ok := mediaTypes[suffix]; ok {
		p.Type = t
	} else {
		p.Type = "unknown"
	}

	id := uuid.New()
	mediaLink, err := saveToGCS(file, id)
	if err != nil {
		http.Error(w, "Failed to save image to GCS", http.StatusInternalServerError)
		fmt.Printf("Failed to save image to GCS %v\n", err)
		return
	}
	p.Url = mediaLink

	if p.Type == "image" {
		uri := fmt.Sprintf("gs://%s/%s", BUCKET_NAME, id)
		if score, err := annotate(uri); err != nil {
			http.Error(w, "Failed to annotate image", http.StatusInternalServerError)
			fmt.Printf("Failed to annotate the image %v\n", err)
			return
		} else {
			p.Face = score
		}
	}

	err = saveToES(p, POST_INDEX, id)
	if err != nil {
		http.Error(w, "Failed to save post to Elasticsearch", http.StatusInternalServerError)
		fmt.Printf("Failed to save post to Elasticsearch %v\n", err)
		return
	}

}

// Get query results from ES
func readFromES(query elastic.Query, index string) (*elastic.SearchResult, error) {
	client, err := elastic.NewClient(elastic.SetURL(ES_URL))
	if err != nil {
		return nil, err
	}

	searchResult, err := client.Search().
		Index(index).
		Query(query).
		Pretty(true).
		Do(context.Background())
	if err != nil {
		return nil, err
	}

	return searchResult, nil
}

// Get complete query results
// Convert query result to a list of json
func getPostFromSearchResult(searchResult *elastic.SearchResult) []Post {
	var ptype Post
	var posts []Post

	for _, item := range searchResult.Each(reflect.TypeOf(ptype)) {
		p := item.(Post)
		posts = append(posts, p)
	}
	return posts
}

func handlerCluster(w http.ResponseWriter, r *http.Request) {
	fmt.Println("Received one cluster request")
	w.Header().Set("Content-Type", "application/json")

	term := r.URL.Query().Get("term")
	query := elastic.NewRangeQuery(term).Gte(0.9)

	searchResult, err := readFromES(query, POST_INDEX)
	if err != nil {
		http.Error(w, "Failed to read from Elasticsearch", http.StatusInternalServerError)
		return
	}

	posts := getPostFromSearchResult(searchResult)
	js, err := json.Marshal(posts)
	if err != nil {
		http.Error(w, "Failed to parse post object", http.StatusInternalServerError)
		fmt.Printf("Failed to parse post object %v\n", err)
		return
	}
	w.Write(js)
}

func handlerSearch(w http.ResponseWriter, r *http.Request) {
	fmt.Println("Received one request for search")

	w.Header().Set("Content-Type", "application/json")

	lat, _ := strconv.ParseFloat(r.URL.Query().Get("lat"), 64)
	lon, _ := strconv.ParseFloat(r.URL.Query().Get("lon"), 64)
	// range is optional
	ran := DISTANCE
	if val := r.URL.Query().Get("range"); val != "" {
		ran = val + "km"
	}
	fmt.Println("range is ", ran)

	query := elastic.NewGeoDistanceQuery("location")
	query = query.Distance(ran).Lat(lat).Lon(lon)
	searchResult, err := readFromES(query, POST_INDEX)
	if err != nil {
		http.Error(w, "Failed to read post from Elasticsearch", http.StatusInternalServerError)
		fmt.Printf("Failed to read post from Elasticsearch %v.\n", err)
		return
	}
	posts := getPostFromSearchResult(searchResult)

	js, err := json.Marshal(posts)
	if err != nil {
		http.Error(w, "Failed to parse posts into JSON format", http.StatusInternalServerError)
		fmt.Printf("Failed to parse posts into JSON format %v.\n", err)
		return
	}
	w.Write(js)
}

// save post images to GCS
func saveToGCS(r io.Reader, objectName string) (string, error) {
	ctx := context.Background()
	client, err := storage.NewClient(ctx)
	if err != nil {
		return "", err
	}

	bucket := client.Bucket(BUCKET_NAME)
	if _, err := bucket.Attrs(ctx); err != nil {
		return "", err
	}

	object := bucket.Object(objectName)
	wc := object.NewWriter(ctx)
	if _, err := io.Copy(wc, r); err != nil {
		return "", err
	}

	if err := wc.Close(); err != nil {
		return "", err
	}

	if err := object.ACL().Set(ctx, storage.AllUsers, storage.RoleReader); err != nil {
		return "", err
	}

	attrs, err := object.Attrs(ctx)
	if err != nil {
		return "", err
	}

	fmt.Printf("Image is saved to GCS: %s\n", attrs.MediaLink)
	return attrs.MediaLink, nil
}

func saveToES(post *Post, index string, id string) error {
	client, err := elastic.NewClient(elastic.SetURL(ES_URL))
	if err != nil {
		return err
	}

	_, err = client.Index().
		Index(index).
		Id(id).
		BodyJson(post).
		Do(context.Background())

	if err != nil {
		return err
	}

	fmt.Printf("Post is saved to index: %s\n", post.Message)
	return nil
}
