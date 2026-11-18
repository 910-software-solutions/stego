// Package documentcard is the fill of the query document-card: it reads one Document.
package documentcard

import (
	"context"

	queries "example.com/shop/out/queries"
)

// Card is the answer of document-card.
type Card struct {
	ID    string `json:"id"`
	Title string `json:"title"`
}

// Fill reads the Document of the request.
func Fill(ctx context.Context, req queries.DocumentCardRequest, h *queries.DocumentCardHandle) (queries.DocumentCardResponse, error) {
	documents, err := queries.DocumentCardDocuments(h)
	if err != nil {
		return nil, err
	}
	d, err := documents.Get(ctx, req.DocumentID)
	if err != nil {
		return nil, err
	}
	return Card{ID: d.Value().ID, Title: d.Value().Title}, nil
}
