// pkg/models/blog.go

package models

type Author struct {
	Name   string `json:"name"`
	Avatar string `json:"avatar"`
}

type BlogPost struct {
	ID       string   `json:"id"`
	Title    string   `json:"title"`
	Excerpt  string   `json:"excerpt"`
	Content  string   `json:"content"`
	Category string   `json:"category"`
	ReadTime string   `json:"readTime"`
	Date     string   `json:"date"`
	Tags     []string `json:"tags"`
	Image    string   `json:"image"`
	Author   Author   `json:"author"`
}