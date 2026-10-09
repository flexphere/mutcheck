// Package model は yo の生成コードを模したもの。
package model

import (
	"context"

	"cloud.google.com/go/spanner"
)

type Parent struct {
	OrgID, ParentID, Name string
	Status                int64
}

func (p *Parent) Insert(ctx context.Context) *spanner.Mutation {
	return spanner.Insert("Parent", []string{"OrgID", "ParentID", "Name", "Status"}, []any{p.OrgID, p.ParentID, p.Name, p.Status})
}

func (p *Parent) Update(ctx context.Context) *spanner.Mutation {
	return spanner.Update("Parent", []string{"OrgID", "ParentID", "Name", "Status"}, []any{p.OrgID, p.ParentID, p.Name, p.Status})
}

func (p *Parent) Delete(ctx context.Context) *spanner.Mutation {
	return spanner.Delete("Parent", spanner.Key{p.OrgID, p.ParentID})
}

type Child struct {
	OrgID, ParentID, ChildID, Value string
}

func (c *Child) Insert(ctx context.Context) *spanner.Mutation {
	return spanner.Insert("Child", []string{"OrgID", "ParentID", "ChildID", "Value"}, []any{c.OrgID, c.ParentID, c.ChildID, c.Value})
}

type Gen struct {
	ID, Phone string
	Count     int64
}

func (g *Gen) Insert(ctx context.Context) *spanner.Mutation {
	return spanner.Insert("Gen", []string{"ID", "Phone", "Count"}, []any{g.ID, g.Phone, g.Count})
}
