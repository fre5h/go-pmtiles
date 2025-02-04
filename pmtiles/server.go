package pmtiles

import (
	"bytes"
	"container/list"
	"context"
	"fmt"
	"gocloud.dev/blob"
	"io"
	"log"
	"regexp"
	"strconv"
	"strings"
	"path"
	"os"
)

type CacheKey struct {
	name   string
	offset uint64 // is 0 for header
	length uint64 // is 0 for header
}

type Request struct {
	key   CacheKey
	value chan CachedValue
}

type CachedValue struct {
	header    HeaderV3
	directory []EntryV3
	etag      string
	ok        bool
}

type Response struct {
	key   CacheKey
	value CachedValue
	size  int
	ok    bool
}

// type HTTPFetcher struct {
// 	bucket string
// 	client *http.Client
// }

// func (fetcher HTTPFetcher) Do(key Key, readFunc func(io.Reader)) bool {
// 	archive := fetcher.bucket + "/" + key.name + ".pmtiles"
// 	fetch, _ := http.NewRequest("GET", archive, nil)
// 	end := key.rng.Offset + uint64(key.rng.Length) - 1
// 	range_header := fmt.Sprintf("bytes=%d-%d", key.rng.Offset, end)
// 	fetch.Header.Add("Range", range_header)
// 	fetch_resp, err := fetcher.client.Do(fetch)
// 	if err != nil || fetch_resp.StatusCode >= 300 || fetch_resp.ContentLength != int64(key.rng.Length) {
// 		return false
// 	}
// 	defer fetch_resp.Body.Close()
// 	readFunc(fetch_resp.Body)
// 	return true
// }

type Server struct {
	reqs      chan Request
	bucket    *blob.Bucket
	logger    *log.Logger
	cacheSize int
	cors      string
}

func NewServer(bucketURL string, prefix string, logger *log.Logger, cacheSize int, cors string) (*Server, error) {
	if bucketURL == "" {
		if strings.HasPrefix(prefix,"/") {
			bucketURL = "file:///"
		} else {
			bucketURL = "file://"
		}
	}

	reqs := make(chan Request, 8)

	ctx := context.Background()

	bucket, err := blob.OpenBucket(ctx, bucketURL)


	if prefix != "/" && prefix != "." {
		bucket = blob.PrefixedBucket(bucket, path.Clean(prefix) + string(os.PathSeparator))
	}


	if err != nil {
		return nil, fmt.Errorf("Failed to open bucket for %s, %w", prefix, err)
	}

	l := &Server{
		reqs:      reqs,
		bucket:    bucket,
		logger:    logger,
		cacheSize: cacheSize,
		cors:      cors,
	}

	return l, nil
}

func (server *Server) Start() {
	go func() {
		cache := make(map[CacheKey]*list.Element)
		inflight := make(map[CacheKey][]Request)
		resps := make(chan Response, 8)
		evictList := list.New()
		totalSize := 0
		ctx := context.Background()

		for {
			select {
			case req := <-server.reqs:
				key := req.key
				if val, ok := cache[key]; ok {
					evictList.MoveToFront(val)
					req.value <- val.Value.(*Response).value
				} else if _, ok := inflight[key]; ok {
					inflight[key] = append(inflight[key], req)
				} else {
					inflight[key] = []Request{req}
					go func() {
						var result CachedValue
						is_root := (key.offset == 0 && key.length == 0)

						offset := int64(key.offset)
						length := int64(key.length)

						if is_root {
							offset = 0
							length = 16384
						}

						server.logger.Printf("fetching %s %d-%d", key.name, offset, length)
						r, err := server.bucket.NewRangeReader(ctx, key.name+".pmtiles", offset, length, nil)

						// TODO: store away ETag
						if err != nil {
							ok = false
							resps <- Response{key: key, value: result}
							server.logger.Printf("failed to fetch %s %d-%d, %v", key.name, key.offset, key.length, err)
							return
						}
						defer r.Close()
						b, err := io.ReadAll(r)
						if err != nil {
							ok = false
							resps <- Response{key: key, value: result}
							server.logger.Printf("failed to fetch %s %d-%d, %v", key.name, key.offset, key.length, err)
							return
						}

						if is_root {
							header, err := deserialize_header(b[0:HEADERV3_LEN_BYTES])
							if err != nil {
								server.logger.Printf("parsing header failed: %v", err)
								return
							}

							// populate the root first before header
							root_entries := deserialize_entries(bytes.NewBuffer(b[header.RootOffset : header.RootOffset+header.RootLength]))
							result2 := CachedValue{directory: root_entries, ok: true}

							root_key := CacheKey{name: key.name, offset: header.RootOffset, length: header.RootLength}
							resps <- Response{key: root_key, value: result2, size: 24 * len(root_entries), ok: true}

							result = CachedValue{header: header, ok: true}
							resps <- Response{key: key, value: result, size: 127, ok: true}
						} else {
							directory := deserialize_entries(bytes.NewBuffer(b))
							result = CachedValue{directory: directory, ok: true}
							resps <- Response{key: key, value: result, size: 24 * len(directory), ok: true}
						}

						server.logger.Printf("fetched %s %d-%d", key.name, key.offset, key.length)
					}()
				}
			case resp := <-resps:
				key := resp.key
				// check if there are any requests waiting on the key
				for _, v := range inflight[key] {
					v.value <- resp.value
				}
				delete(inflight, key)

				if resp.ok {
					totalSize += resp.size
					ent := &resp
					entry := evictList.PushFront(ent)
					cache[key] = entry

					for {
						if totalSize < server.cacheSize*1000*1000 {
							break
						}
						ent := evictList.Back()
						if ent != nil {
							evictList.Remove(ent)
							kv := ent.Value.(*Response)
							delete(cache, kv.key)
							totalSize -= kv.size
						}
					}
				}
			}
		}
	}()
}

func (server *Server) get_metadata(ctx context.Context, http_headers map[string]string, name string) (int, map[string]string, []byte) {
	root_req := Request{key: CacheKey{name: name, offset: 0, length: 0}, value: make(chan CachedValue, 1)}
	server.reqs <- root_req
	root_value := <-root_req.value
	header := root_value.header

	if !root_value.ok {
		return 404, http_headers, []byte("Archive not found")
	}

	r, err := server.bucket.NewRangeReader(ctx, name+".pmtiles", int64(header.MetadataOffset), int64(header.MetadataLength), nil)
	if err != nil {
		return 404, http_headers, []byte("Archive not found")
	}
	defer r.Close()
	b, err := io.ReadAll(r)
	if err != nil {
		return 500, http_headers, []byte("I/O Error")
	}

	if header_val, ok := headerContentEncoding(header.InternalCompression); ok {
		http_headers["Content-Encoding"] = header_val
	}
	http_headers["Content-Type"] = "application/json"

	return 200, http_headers, b
}

func (server *Server) get_tile(ctx context.Context, http_headers map[string]string, name string, z uint8, x uint32, y uint32, ext string) (int, map[string]string, []byte) {
	root_req := Request{key: CacheKey{name: name, offset: 0, length: 0}, value: make(chan CachedValue, 1)}
	server.reqs <- root_req

	// https://golang.org/doc/faq#atomic_maps
	root_value := <-root_req.value
	header := root_value.header

	if !root_value.ok {
		return 404, http_headers, []byte("Archive not found")
	}

	if z < header.MinZoom || z > header.MaxZoom {
		return 404, http_headers, []byte("Tile not found")
	}

	switch header.TileType {
	case Mvt:
		if ext != "mvt" {
			return 400, http_headers, []byte("path mismatch: archive is type MVT (.mvt)")
		}
	case Png:
		if ext != "png" {
			return 400, http_headers, []byte("path mismatch: archive is type PNG (.png)")
		}
	case Jpeg:
		if ext != "jpg" {
			return 400, http_headers, []byte("path mismatch: archive is type JPEG (.jpg)")
		}
	case Webp:
		if ext != "webp" {
			return 400, http_headers, []byte("path mismatch: archive is type WebP (.webp)")
		}
	}

	tile_id := ZxyToId(z, x, y)
	dir_offset, dir_len := header.RootOffset, header.RootLength

	for depth := 0; depth <= 3; depth++ {
		dir_req := Request{key: CacheKey{name: name, offset: dir_offset, length: dir_len}, value: make(chan CachedValue, 1)}
		server.reqs <- dir_req
		dir_value := <-dir_req.value
		directory := dir_value.directory
		entry, ok := find_tile(directory, tile_id)
		if ok {
			if entry.RunLength > 0 {
				r, err := server.bucket.NewRangeReader(ctx, name+".pmtiles", int64(header.TileDataOffset+entry.Offset), int64(entry.Length), nil)
				if err != nil {
					return 500, http_headers, []byte("Network error")
				}
				defer r.Close()
				b, err := io.ReadAll(r)
				if err != nil {
					return 500, http_headers, []byte("I/O error")
				}
				if header_val, ok := headerContentType(header); ok {
					http_headers["Content-Type"] = header_val
				}
				if header_val, ok := headerContentEncoding(header.TileCompression); ok {
					http_headers["Content-Encoding"] = header_val
				}
				return 200, http_headers, b
			} else {
				dir_offset = header.LeafDirectoryOffset + entry.Offset
				dir_len = uint64(entry.Length)
			}
		} else {
			break
		}
	}

	return 204, http_headers, nil
}

var tilePattern = regexp.MustCompile(`^\/([-A-Za-z0-9_\/!-_\.\*'\(\)']+)\/(\d+)\/(\d+)\/(\d+)\.([a-z]+)$`)
var metadataPattern = regexp.MustCompile(`^\/([-A-Za-z0-9_\/!-_\.\*'\(\)']+)\/metadata$`)

func parse_tile_path(path string) (bool, string, uint8, uint32, uint32, string) {
	if res := tilePattern.FindStringSubmatch(path); res != nil {
		name := res[1]
		z, _ := strconv.ParseUint(res[2], 10, 8)
		x, _ := strconv.ParseUint(res[3], 10, 32)
		y, _ := strconv.ParseUint(res[4], 10, 32)
		ext := res[5]
		return true, name, uint8(z), uint32(x), uint32(y), ext
	}
	return false, "", 0, 0, 0, ""
}

func parse_metadata_path(path string) (bool, string) {
	if res := metadataPattern.FindStringSubmatch(path); res != nil {
		name := res[1]
		return true, name
	}
	return false, ""
}

func (server *Server) Get(ctx context.Context, path string) (int, map[string]string, []byte) {
	http_headers := make(map[string]string)
	if len(server.cors) > 0 {
		http_headers["Access-Control-Allow-Origin"] = server.cors
	}

	if ok, key, z, x, y, ext := parse_tile_path(path); ok {
		return server.get_tile(ctx, http_headers, key, z, x, y, ext)
	}
	if ok, key := parse_metadata_path(path); ok {
		return server.get_metadata(ctx, http_headers, key)
	}

	return 404, http_headers, []byte("Tile not found")
}
