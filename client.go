package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// Client is the thin HTTP layer over the local any server. Every method
// maps 1:1 onto one route; shapes follow any's docs/03-api.md.
type Client struct {
	base string
	hc   *http.Client
}

func NewClient() *Client {
	addr := os.Getenv("ANY_AGENTS_ADDR")
	if addr == "" {
		addr = "127.0.0.1:2046"
	}
	return &Client{base: "http://" + addr, hc: &http.Client{Timeout: 90 * time.Second}}
}

type apiError struct {
	Status  int
	Code    string
	Message string
	Details map[string]any
}

func (e *apiError) Error() string {
	return fmt.Sprintf("server %d %s: %s", e.Status, e.Code, e.Message)
}

// isCode reports whether err is an any error with the given code.
func isCode(err error, code string) bool {
	ae, ok := err.(*apiError)
	return ok && ae.Code == code
}

func (c *Client) do(method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, c.base+path, body)
	if err != nil {
		return err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return decodeError(resp.StatusCode, data)
	}
	if out != nil && len(data) > 0 {
		return json.Unmarshal(data, out)
	}
	return nil
}

func decodeError(status int, data []byte) error {
	var env struct {
		Error struct {
			Code    string         `json:"code"`
			Message string         `json:"message"`
			Details map[string]any `json:"details"`
		} `json:"error"`
	}
	_ = json.Unmarshal(data, &env)
	if env.Error.Code == "" {
		env.Error.Message = strings.TrimSpace(string(data))
	}
	return &apiError{Status: status, Code: env.Error.Code, Message: env.Error.Message, Details: env.Error.Details}
}

func seg(s string) string { return url.PathEscape(s) }

func (c *Client) Health() error { return c.do("GET", "/v1/health", nil, nil) }

// --- account / spaces ---

type Account struct {
	Id          string `json:"id"`
	TechSpaceId string `json:"techSpaceId"`
}

func (c *Client) Account() (*Account, error) {
	var out Account
	return &out, c.do("GET", "/v1/account", nil, &out)
}

type SpaceInfo struct {
	Id                 string `json:"id"`
	Name               string `json:"name"`
	SpaceType          string `json:"spaceType"`
	OwnRole            string `json:"ownRole"`
	Status             string `json:"status"`
	SpaceIndexObjectId string `json:"spaceIndexObjectId"`
}

// GetSpace reads one space row (a member's view, any status).
func (c *Client) GetSpace(space string) (*SpaceInfo, error) {
	var out SpaceInfo
	return &out, c.do("GET", "/v1/spaces/"+seg(space), nil, &out)
}

// --- members ---

// Member is one row of a space's roster: identity, role and profile.
type Member struct {
	Identity   string `json:"identity"`
	Permission string `json:"permission"`
	Status     string `json:"status"`
	Name       string `json:"name"`
}

func (c *Client) Members(space string) ([]Member, error) {
	var out struct {
		Members []Member `json:"members"`
	}
	return out.Members, c.do("GET", "/v1/spaces/"+seg(space)+"/members", nil, &out)
}

func (c *Client) MemberMe(space string) (*Member, error) {
	var out Member
	return &out, c.do("GET", "/v1/spaces/"+seg(space)+"/members/me", nil, &out)
}

func (c *Client) ListSpaces() ([]SpaceInfo, error) {
	var out struct {
		Spaces []SpaceInfo `json:"spaces"`
	}
	return out.Spaces, c.do("GET", "/v1/spaces", nil, &out)
}

func (c *Client) CreateSpace(name, description string) (string, error) {
	var out struct {
		Id string `json:"id"`
	}
	return out.Id, c.do("POST", "/v1/spaces", map[string]string{"name": name, "description": description}, &out)
}

func (c *Client) DeleteSpace(space string) error {
	return c.do("DELETE", "/v1/spaces/"+seg(space), nil, nil)
}

// --- bundles ---

type Bundle struct {
	Id      string   `json:"id"`
	Name    string   `json:"name"`
	RootId  string   `json:"rootId"`
	Roots   []string `json:"roots"`
	Losers  []string `json:"losers"`
	Derived bool     `json:"derived"`
}

// BundleEnsure is the adopt-or-install request. xKey + properties (+
// parts, layout) declare a type on the root; Collection declares a
// collection instead (no parts, no layout).
type BundleEnsure struct {
	Id              string           `json:"id"`
	Name            string           `json:"name,omitempty"`
	XKey            string           `json:"xKey,omitempty"`
	Layout          map[string]any   `json:"layout,omitempty"`
	Hidden          bool             `json:"hidden,omitempty"`
	Collection      bool             `json:"collection,omitempty"`
	RootType        string           `json:"rootType,omitempty"`
	RootCollections []string         `json:"rootCollections,omitempty"`
	RootProperties  map[string]any   `json:"rootProperties,omitempty"`
	Properties      []PropertyDraft  `json:"properties,omitempty"`
	Parts           []map[string]any `json:"parts,omitempty"`
}

func (c *Client) EnsureBundle(space string, req *BundleEnsure) (*Bundle, bool, error) {
	var out struct {
		Bundle    Bundle `json:"bundle"`
		Installed bool   `json:"installed"`
	}
	err := c.do("POST", "/v1/spaces/"+seg(space)+"/bundles", req, &out)
	return &out.Bundle, out.Installed, err
}

func (c *Client) ListBundles(space string) ([]Bundle, bool, error) {
	var out struct {
		Bundles []Bundle `json:"bundles"`
		Synced  bool     `json:"synced"`
	}
	err := c.do("GET", "/v1/spaces/"+seg(space)+"/bundles", nil, &out)
	return out.Bundles, out.Synced, err
}

func (c *Client) GetBundle(space, id string) (*Bundle, error) {
	var out struct {
		Bundle Bundle `json:"bundle"`
	}
	err := c.do("GET", "/v1/spaces/"+seg(space)+"/bundles/"+seg(id), nil, &out)
	return &out.Bundle, err
}

// BundleChild mints (or resolves) the deterministic child of a bundle root.
func (c *Client) BundleChild(space, bundleId, seed, typeId string, collections []string) (string, error) {
	var out struct {
		ObjectId string `json:"objectId"`
	}
	body := map[string]any{"seed": seed, "type": typeId}
	if len(collections) > 0 {
		body["collections"] = collections
	}
	return out.ObjectId, c.do("POST", "/v1/spaces/"+seg(space)+"/bundles/"+seg(bundleId)+"/children", body, &out)
}

// --- catalog ---

type CatalogBundleResult struct {
	Usecase    string            `json:"usecase"`
	Id         string            `json:"id"`
	Bundle     Bundle            `json:"bundle"`
	Installed  bool              `json:"installed"`
	TypeId     string            `json:"typeId"`
	Properties map[string]string `json:"properties"`
}

func (c *Client) CatalogSetup(space, usecase string) ([]CatalogBundleResult, error) {
	var out struct {
		Bundles []CatalogBundleResult `json:"bundles"`
	}
	return out.Bundles, c.do("POST", "/v1/catalog/"+seg(usecase)+"/setup", map[string]string{"spaceId": space}, &out)
}

// --- types, properties, parts ---

type TypeInfo struct {
	Id      string         `json:"id"`
	XKey    string         `json:"xKey"`
	Name    string         `json:"name"`
	Weight  int            `json:"weight"`
	Layout  map[string]any `json:"layout"`
	Hidden  bool           `json:"hidden"`
	BuiltIn bool           `json:"builtIn"`
}

func (c *Client) ListTypes(space string) ([]TypeInfo, error) {
	var out struct {
		Types []TypeInfo `json:"types"`
	}
	return out.Types, c.do("GET", "/v1/spaces/"+seg(space)+"/types?includeHidden=true", nil, &out)
}

type PropertyDraft struct {
	XKey        string         `json:"xKey"`
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Kind        string         `json:"kind"`
	Meta        map[string]any `json:"meta,omitempty"`
	XFormat     map[string]any `json:"xFormat,omitempty"`
}

type PropertyInfo struct {
	Id      string         `json:"id"`
	XKey    string         `json:"xKey"`
	Name    string         `json:"name"`
	Kind    string         `json:"kind"`
	XFormat map[string]any `json:"xFormat"`
}

func (c *Client) ListProperties(space, typeId string) ([]PropertyInfo, error) {
	var out struct {
		Properties []PropertyInfo `json:"properties"`
	}
	return out.Properties, c.do("GET", "/v1/spaces/"+seg(space)+"/types/"+seg(typeId)+"/properties", nil, &out)
}

func (c *Client) AddProperty(space, typeId string, d PropertyDraft) (string, error) {
	var out struct {
		PropId string `json:"propId"`
	}
	return out.PropId, c.do("POST", "/v1/spaces/"+seg(space)+"/types/"+seg(typeId)+"/properties", d, &out)
}

type PartDataset struct {
	Id         string `json:"id"`
	Key        string `json:"key"`
	Collection string `json:"collection"`
	Module     string `json:"module"`
	Shared     bool   `json:"shared"`
}

type PartInfo struct {
	Id       string        `json:"id"`
	Key      string        `json:"key"`
	Name     string        `json:"name"`
	Datasets []PartDataset `json:"datasets"`
}

func (c *Client) ListParts(space, typeId string) ([]PartInfo, error) {
	var out struct {
		Parts []PartInfo `json:"parts"`
	}
	return out.Parts, c.do("GET", "/v1/spaces/"+seg(space)+"/types/"+seg(typeId)+"/parts", nil, &out)
}

func (c *Client) PatchType(space, typeId string, patch map[string]any) error {
	return c.do("PATCH", "/v1/spaces/"+seg(space)+"/types/"+seg(typeId), patch, nil)
}

// ListCollectionProperties resolves a collection's columns (xKey → id).
// PatchCollection updates a collection's name, description, hidden or meta.
func (c *Client) PatchCollection(space, collectionId string, patch map[string]any) error {
	return c.do("PATCH", "/v1/spaces/"+seg(space)+"/collections/"+seg(collectionId), patch, nil)
}

func (c *Client) ListCollectionProperties(space, collectionId string) ([]PropertyInfo, error) {
	var out struct {
		Properties []PropertyInfo `json:"properties"`
	}
	return out.Properties, c.do("GET", "/v1/spaces/"+seg(space)+"/collections/"+seg(collectionId)+"/properties", nil, &out)
}

// --- objects ---

// CreateObject mints an object of one type, filed under collections;
// initial is keyed by owner (the type, a collection, or "any").
func (c *Client) CreateObject(space, typeId string, collections []string, initial map[string]any) (string, error) {
	var out struct {
		ObjectId string `json:"objectId"`
	}
	body := map[string]any{"type": typeId}
	if len(collections) > 0 {
		body["collections"] = collections
	}
	if len(initial) > 0 {
		body["initialProperties"] = initial
	}
	return out.ObjectId, c.do("POST", "/v1/spaces/"+seg(space)+"/objects", body, &out)
}

func (c *Client) GetObject(space, objectId string) (map[string]any, error) {
	var out struct {
		Record map[string]any `json:"record"`
	}
	return out.Record, c.do("GET", "/v1/spaces/"+seg(space)+"/objects/"+seg(objectId), nil, &out)
}

func (c *Client) DeleteObject(space, objectId string) error {
	return c.do("DELETE", "/v1/spaces/"+seg(space)+"/objects/"+seg(objectId), nil, nil)
}

// SetProperties writes values under one owner (type or collection): {propId: value}.
func (c *Client) SetProperties(space, objectId, ownerId string, patch map[string]any) error {
	return c.do("POST", "/v1/spaces/"+seg(space)+"/properties/"+seg(objectId)+"/set/"+seg(ownerId),
		map[string]any{"patch": patch}, nil)
}

// AttachCollection files an object under a collection (idempotent).
func (c *Client) AttachCollection(space, objectId, collectionId string) error {
	return c.do("POST", "/v1/spaces/"+seg(space)+"/properties/"+seg(objectId)+"/collections/"+seg(collectionId), nil, nil)
}

func (c *Client) DetachCollection(space, objectId, collectionId string) error {
	return c.do("DELETE", "/v1/spaces/"+seg(space)+"/properties/"+seg(objectId)+"/collections/"+seg(collectionId), nil, nil)
}

type Query struct {
	ObjectId       string         `json:"objectId,omitempty"`
	Dataset        string         `json:"dataset,omitempty"`
	Filter         map[string]any `json:"filter,omitempty"`
	Sort           []string       `json:"sort,omitempty"`
	Limit          int            `json:"limit,omitempty"`
	Offset         int            `json:"offset,omitempty"`
	IncludeTotal   bool           `json:"includeTotal,omitempty"`
	IncludeDeleted bool           `json:"includeDeleted,omitempty"`
	Projection     map[string]any `json:"projection,omitempty"`
}

type queryResult struct {
	Records []map[string]any `json:"records"`
	Total   int              `json:"total"`
	HasNext bool             `json:"hasNext"`
}

// QueryObjects reads the cross-object collection (one row per object).
func (c *Client) QueryObjects(space string, q Query) ([]map[string]any, error) {
	var out queryResult
	err := c.do("POST", "/v1/spaces/"+seg(space)+"/objects/query", q, &out)
	return out.Records, err
}

// QueryObjectsAll pages through every matching object row.
func (c *Client) QueryObjectsAll(space string, q Query) ([]map[string]any, error) {
	const page = 500
	q.Limit, q.Offset = page, 0
	var all []map[string]any
	for {
		var out queryResult
		if err := c.do("POST", "/v1/spaces/"+seg(space)+"/objects/query", q, &out); err != nil {
			return nil, err
		}
		all = append(all, out.Records...)
		if len(out.Records) < page {
			return all, nil
		}
		q.Offset += page
	}
}

// QueryRecords reads one object's dataset.
func (c *Client) QueryRecords(space, objectId, dataset string, filter map[string]any, sort []string, limit int) ([]map[string]any, error) {
	var out queryResult
	q := Query{ObjectId: objectId, Dataset: dataset, Filter: filter, Sort: sort, Limit: limit}
	err := c.do("POST", "/v1/spaces/"+seg(space)+"/query", q, &out)
	return out.Records, err
}

func (c *Client) QueryRecordsAll(space, objectId, dataset string, filter map[string]any, sort []string) ([]map[string]any, error) {
	const page = 500
	q := Query{ObjectId: objectId, Dataset: dataset, Filter: filter, Sort: sort, Limit: page}
	var all []map[string]any
	for {
		var out queryResult
		if err := c.do("POST", "/v1/spaces/"+seg(space)+"/query", q, &out); err != nil {
			return nil, err
		}
		all = append(all, out.Records...)
		if len(out.Records) < page {
			return all, nil
		}
		q.Offset += page
	}
}

type UpsertRecord struct {
	Id     string         `json:"id"`
	Fields map[string]any `json:"fields"`
}

type UpsertResult struct {
	Created    int `json:"created"`
	Updated    int `json:"updated"`
	Skipped    int `json:"skipped"`
	Rejections []struct {
		Index  int    `json:"index"`
		Id     string `json:"id"`
		Code   string `json:"code"`
		Reason string `json:"reason"`
	} `json:"rejections"`
}

func (c *Client) Upsert(space, objectId, dataset string, records []UpsertRecord) (*UpsertResult, error) {
	var out UpsertResult
	body := map[string]any{"objectId": objectId, "dataset": dataset, "records": records, "traceIds": []string{"any-agents"}}
	return &out, c.do("POST", "/v1/spaces/"+seg(space)+"/upsert", body, &out)
}

type ModifyOp struct {
	Type  string `json:"type"`
	Path  string `json:"path"`
	Value any    `json:"value,omitempty"`
}

type ModifyRecord struct {
	Id     string     `json:"id"`
	Upsert bool       `json:"upsert,omitempty"`
	Ops    []ModifyOp `json:"ops"`
}

type ModifyResult struct {
	Rejections []struct {
		Id     string `json:"id"`
		Code   string `json:"code"`
		Reason string `json:"reason"`
	} `json:"rejections"`
}

func (c *Client) Modify(space, objectId, dataset string, records []ModifyRecord) (*ModifyResult, error) {
	var out ModifyResult
	body := map[string]any{"objectId": objectId, "dataset": dataset, "records": records}
	return &out, c.do("POST", "/v1/spaces/"+seg(space)+"/modify", body, &out)
}

func (c *Client) DeleteRecords(space, objectId, dataset string, ids []string) error {
	body := map[string]any{"objectId": objectId, "dataset": dataset, "recordIds": ids}
	return c.do("POST", "/v1/spaces/"+seg(space)+"/delete-records", body, nil)
}

func (c *Client) Aggregate(space, objectId, dataset string, pipeline []map[string]any) ([]map[string]any, error) {
	var out struct {
		Records []map[string]any `json:"records"`
	}
	body := map[string]any{"objectId": objectId, "dataset": dataset, "pipeline": pipeline}
	return out.Records, c.do("POST", "/v1/spaces/"+seg(space)+"/aggregate", body, &out)
}

func (c *Client) AggregateObjects(space string, pipeline []map[string]any) ([]map[string]any, error) {
	var out struct {
		Records []map[string]any `json:"records"`
	}
	return out.Records, c.do("POST", "/v1/spaces/"+seg(space)+"/objects/aggregate", map[string]any{"pipeline": pipeline}, &out)
}

// --- search ---

type SearchHit struct {
	Scope    string  `json:"scope"`
	ObjectId string  `json:"objectId"`
	Dataset  string  `json:"dataset"`
	RecordId string  `json:"recordId"`
	Data     string  `json:"data"`
	Score    float64 `json:"score"`
}

type SearchResult struct {
	Hits         []SearchHit `json:"hits"`
	Mode         string      `json:"mode"`
	VectorStatus string      `json:"vectorStatus"`
}

func (c *Client) Search(space, query string, limit int) (*SearchResult, error) {
	var out SearchResult
	body := map[string]any{"query": query, "limit": limit, "maxData": 700}
	return &out, c.do("POST", "/v1/spaces/"+seg(space)+"/search", body, &out)
}

// --- backlinks ---

type LinkEdge struct {
	Source struct {
		ObjectId string `json:"objectId"`
		Dataset  string `json:"dataset"`
		RecordId string `json:"recordId"`
		TypeId   string `json:"typeId"`
	} `json:"source"`
	Kind string `json:"kind"`
}

func (c *Client) Backlinks(space, objectId string) ([]LinkEdge, error) {
	var out struct {
		Object []LinkEdge `json:"object"`
		Parts  []LinkEdge `json:"parts"`
	}
	err := c.do("GET", "/v1/spaces/"+seg(space)+"/objects/"+seg(objectId)+"/backlinks?limit=500", nil, &out)
	return append(out.Object, out.Parts...), err
}

// --- editor markdown bridge (shared editor_blocks body) ---

func (c *Client) MarkdownGet(space, objectId string) (string, error) {
	var out struct {
		Content string `json:"content"`
	}
	err := c.do("GET", "/v1/spaces/"+seg(space)+"/objects/"+seg(objectId)+"/editor/editor_blocks/markdown", nil, &out)
	return out.Content, err
}

func (c *Client) MarkdownPut(space, objectId, content string) error {
	return c.do("PUT", "/v1/spaces/"+seg(space)+"/objects/"+seg(objectId)+"/editor/editor_blocks/markdown",
		map[string]string{"content": content}, nil)
}

func (c *Client) MarkdownAppend(space, objectId, content string) error {
	return c.do("POST", "/v1/spaces/"+seg(space)+"/objects/"+seg(objectId)+"/editor/editor_blocks/markdown/append",
		map[string]string{"content": content}, nil)
}

type MarkdownEdit struct {
	OldText    string `json:"oldText"`
	NewText    string `json:"newText"`
	ReplaceAll bool   `json:"replaceAll,omitempty"`
}

func (c *Client) MarkdownPatch(space, objectId string, edits []MarkdownEdit) error {
	return c.do("PATCH", "/v1/spaces/"+seg(space)+"/objects/"+seg(objectId)+"/editor/editor_blocks/markdown",
		map[string]any{"edits": edits}, nil)
}

// --- files ---

type FileInfo struct {
	FileId string `json:"fileId"`
	Name   string `json:"name"`
	Size   int64  `json:"size"`
}

func (c *Client) FileList(space string) ([]FileInfo, error) {
	var out struct {
		Files []FileInfo `json:"files"`
	}
	return out.Files, c.do("GET", "/v1/spaces/"+seg(space)+"/files", nil, &out)
}

func (c *Client) AttachFile(space, objectId, name string, data []byte) (string, error) {
	req, err := http.NewRequest("POST",
		c.base+"/v1/spaces/"+seg(space)+"/objects/"+seg(objectId)+"/files?name="+url.QueryEscape(name),
		bytes.NewReader(data))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := c.hc.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return "", decodeError(resp.StatusCode, b)
	}
	var out struct {
		FileId string `json:"fileId"`
	}
	return out.FileId, json.Unmarshal(b, &out)
}

func (c *Client) DownloadFile(space, fileId string) ([]byte, error) {
	resp, err := c.hc.Get(c.base + "/v1/spaces/" + seg(space) + "/files/" + seg(fileId) + "/content")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		return nil, decodeError(resp.StatusCode, b)
	}
	return b, nil
}

func (c *Client) DeleteFile(space, fileId string) error {
	return c.do("DELETE", "/v1/spaces/"+seg(space)+"/files/"+seg(fileId), nil, nil)
}

// --- devices ---

func (c *Client) RegisterApp(name, version string) error {
	return c.do("PUT", "/v1/devices/me", map[string]any{"apps": map[string]any{name: map[string]any{"version": version}}}, nil)
}

// --- chat (the chat module on a Collab object) ---

// WriteResult is the shared write reply of the module routes.
type WriteResult struct {
	VersionId string   `json:"versionId"`
	ChangeId  string   `json:"changeId"`
	RecordIds []string `json:"recordIds"`
}

// ChatSend posts one message; agent marks it agent-authored so the
// owner's own devices count it unread.
func (c *Client) ChatSend(space, objectId, text string, agent map[string]any) (*WriteResult, error) {
	var out WriteResult
	body := map[string]any{"text": text}
	if agent != nil {
		body["agent"] = agent
	}
	return &out, c.do("POST", "/v1/spaces/"+seg(space)+"/objects/"+seg(objectId)+"/chat/messages", body, &out)
}
