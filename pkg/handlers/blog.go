package handlers

import (
	"yourproject/pkg/models"

	"github.com/gofiber/fiber/v2"
)

type BlogHandler struct {
	// Add any dependencies here
}

func NewBlogHandler() *BlogHandler {
	return &BlogHandler{}
}

func (h *BlogHandler) GetBlogPosts(c *fiber.Ctx) error {
	// Sample data - replace with database query
	posts := []models.BlogPost{
		{
			ID:       "1",
			Title:    "Getting Started with API Testing in Keploy",
			Excerpt:  "Learn how to effectively test your APIs using Keploy's powerful testing framework...",
			Category: "Testing",
			ReadTime: "5 min",
			Date:     "Oct 31, 2024",
			Tags:     []string{"API Testing", "Tutorial", "Beginner"},
			Image:    "/static/images/blog/api-testing.jpg",
			Author: models.Author{
				Name:   "John Doe",
				Avatar: "/static/images/avatars/john.jpg",
			},
		},
		// Add more sample posts
	}

	return c.JSON(fiber.Map{
		"posts": posts,
	})
}

func (h *BlogHandler) GetBlogPost(c *fiber.Ctx) error {
	id := c.Params("id")
	
	// Sample data - replace with database query
	post := models.BlogPost{
		ID:      id,
		Title:   "Getting Started with API Testing in Keploy",
		Content: "Full content here...",
		// ... other fields
	}

	return c.JSON(post)
}