package clientv2

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"reflect"
	"strconv"
	"strings"

	"github.com/99designs/gqlgen/graphql"
	"github.com/Yamashou/gqlgenc/graphqljson"
	"github.com/vektah/gqlparser/v2/gqlerror"
)

type GQLRequestInfo struct {
	Request *Request
}

func NewGQLRequestInfo(r *Request) *GQLRequestInfo {
	return &GQLRequestInfo{
		Request: r,
	}
}

type RequestInterceptorFunc func(ctx context.Context, req *http.Request, gqlInfo *GQLRequestInfo, res interface{}) error

type RequestInterceptor func(ctx context.Context, req *http.Request, gqlInfo *GQLRequestInfo, res interface{}, next RequestInterceptorFunc) error

func ChainInterceptor(interceptors ...RequestInterceptor) RequestInterceptor {
	n := len(interceptors)

	return func(ctx context.Context, req *http.Request, gqlInfo *GQLRequestInfo, res interface{}, next RequestInterceptorFunc) error {
		chainer := func(currentInter RequestInterceptor, currentFunc RequestInterceptorFunc) RequestInterceptorFunc {
			return func(currentCtx context.Context, currentReq *http.Request, currentGqlInfo *GQLRequestInfo, currentRes interface{}) error {
				return currentInter(currentCtx, currentReq, currentGqlInfo, currentRes, currentFunc)
			}
		}

		chainedHandler := next
		for i := n - 1; i >= 0; i-- {
			chainedHandler = chainer(interceptors[i], chainedHandler)
		}

		return chainedHandler(ctx, req, gqlInfo, res)
	}
}

// Client is the http client wrapper
type Client struct {
	Client              *http.Client
	BaseURL             string
	RequestInterceptor  RequestInterceptor
	CustomDo            RequestInterceptorFunc
	ParseDataWhenErrors bool
}

// Request represents an outgoing GraphQL request
type Request struct {
	Query         string                 `json:"query"`
	Variables     map[string]interface{} `json:"variables,omitempty"`
	OperationName string                 `json:"operationName,omitempty"`
}

// NewClient creates a new http client wrapper
func NewClient(client *http.Client, baseURL string, options *Options, interceptors ...RequestInterceptor) *Client {
	c := &Client{
		Client:  client,
		BaseURL: baseURL,
		RequestInterceptor: ChainInterceptor(append([]RequestInterceptor{func(ctx context.Context, requestSet *http.Request, gqlInfo *GQLRequestInfo, res interface{}, next RequestInterceptorFunc) error {
			return next(ctx, requestSet, gqlInfo, res)
		}}, interceptors...)...),
	}

	if options != nil {
		c.ParseDataWhenErrors = options.ParseDataAlongWithErrors
	}

	return c
}

// Options is a struct that holds some client-specific options that can be passed to NewClient.
type Options struct {
	// ParseDataAlongWithErrors is a flag that indicates whether the client should try to parse and return the data along with error
	// when error appeared. So in the end you'll get list of gql errors and data.
	ParseDataAlongWithErrors bool
}

// GqlErrorList is the struct of a standard graphql error response
type GqlErrorList struct {
	Errors gqlerror.List `json:"errors"`
}

func (e *GqlErrorList) Error() string {
	return e.Errors.Error()
}

// HTTPError is the error when a GqlErrorList cannot be parsed
type HTTPError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// ErrorResponse represent an handled error
type ErrorResponse struct {
	// populated when http status code is not OK
	NetworkError *HTTPError `json:"networkErrors"`
	// populated when http status code is OK but the server returned at least one graphql error
	GqlErrors *gqlerror.List `json:"graphqlErrors"`
}

// HasErrors returns true when at least one error is declared
func (er *ErrorResponse) HasErrors() bool {
	return er.NetworkError != nil || er.GqlErrors != nil
}

func (er *ErrorResponse) Error() string {
	content, err := json.Marshal(er)
	if err != nil {
		return err.Error()
	}

	return string(content)
}

type MultipartFile struct {
	File  graphql.Upload
	Index int
}

type MultipartFilesGroup struct {
	Files      []MultipartFile
	IsMultiple bool
}

type FormField struct {
	Name  string
	Value interface{}
}

type header struct {
	key, value string
}

// Post support send multipart form with files https://gqlgen.com/reference/file-upload/ https://github.com/jaydenseric/graphql-multipart-request-spec
func (c *Client) Post(ctx context.Context, operationName, query string, respData interface{}, vars map[string]interface{}, interceptors ...RequestInterceptor) error {
	multipartFilesGroups, mapping, vars := parseMultipartFiles(vars)

	r := &Request{
		Query:         query,
		Variables:     vars,
		OperationName: operationName,
	}

	gqlInfo := NewGQLRequestInfo(r)
	body := new(bytes.Buffer)

	var headers []header

	if len(multipartFilesGroups) > 0 {
		contentType, err := prepareMultipartFormBody(
			body,
			[]FormField{
				{
					Name:  "operations",
					Value: r,
				},
				{
					Name:  "map",
					Value: mapping,
				},
			},
			multipartFilesGroups,
		)
		if err != nil {
			return fmt.Errorf("failed to prepare form body: %w", err)
		}

		headers = append(headers, header{key: "Content-Type", value: contentType})
	} else {
		requestBody, err := MarshalJSON(r)
		if err != nil {
			return fmt.Errorf("encode: %w", err)
		}

		body = bytes.NewBuffer(requestBody)

		headers = append(headers, header{key: "Content-Type", value: "application/json; charset=utf-8"})
		headers = append(headers, header{key: "Accept", value: "application/json; charset=utf-8"})
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL, body)
	if err != nil {
		return fmt.Errorf("create request struct failed: %w", err)
	}

	for _, h := range headers {
		req.Header.Set(h.key, h.value)
	}

	f := ChainInterceptor(append([]RequestInterceptor{c.RequestInterceptor}, interceptors...)...)

	// if custom do is set, use it instead of the default one
	if c.CustomDo != nil {
		return f(ctx, req, gqlInfo, respData, c.CustomDo)
	}

	return f(ctx, req, gqlInfo, respData, c.do)
}

func parseMultipartFiles(
	vars map[string]interface{},
) ([]MultipartFilesGroup, map[string][]string, map[string]interface{}) {
	var (
		multipartFilesGroups []MultipartFilesGroup
		mapping              = map[string][]string{}
		i                    = 0
	)

	for k, v := range vars {
		switch item := v.(type) {
		case graphql.Upload:
			iStr := strconv.Itoa(i)
			vars[k] = nil
			mapping[iStr] = []string{fmt.Sprintf("variables.%s", k)}

			multipartFilesGroups = append(multipartFilesGroups, MultipartFilesGroup{
				Files: []MultipartFile{
					{
						Index: i,
						File:  item,
					},
				},
			})

			i++
		case []*graphql.Upload:
			vars[k] = make([]struct{}, len(item))
			var groupFiles []MultipartFile

			for itemI, itemV := range item {
				iStr := strconv.Itoa(i)
				mapping[iStr] = []string{fmt.Sprintf("variables.%s.%s", k, strconv.Itoa(itemI))}

				groupFiles = append(groupFiles, MultipartFile{
					Index: i,
					File:  *itemV,
				})

				i++
			}

			multipartFilesGroups = append(multipartFilesGroups, MultipartFilesGroup{
				Files:      groupFiles,
				IsMultiple: true,
			})
		}
	}

	return multipartFilesGroups, mapping, vars
}

func prepareMultipartFormBody(
	buffer *bytes.Buffer, formFields []FormField, files []MultipartFilesGroup,
) (string, error) {
	writer := multipart.NewWriter(buffer)
	defer writer.Close()

	// form fields
	for _, field := range formFields {
		fieldBody, err := json.Marshal(field.Value)
		if err != nil {
			return "", fmt.Errorf("encode %s: %w", field.Name, err)
		}

		err = writer.WriteField(field.Name, string(fieldBody))
		if err != nil {
			return "", fmt.Errorf("write %s: %w", field.Name, err)
		}
	}

	// files
	for _, filesGroup := range files {
		for _, file := range filesGroup.Files {
			part, err := writer.CreateFormFile(strconv.Itoa(file.Index), file.File.Filename)
			if err != nil {
				return "", fmt.Errorf("form file %w", err)
			}

			_, err = io.Copy(part, file.File.File)
			if err != nil {
				return "", fmt.Errorf("copy file %w", err)
			}
		}
	}

	if err := writer.Close(); err != nil {
		return "", fmt.Errorf("writer close %w", err)
	}

	return writer.FormDataContentType(), nil
}

func (c *Client) do(_ context.Context, req *http.Request, _ *GQLRequestInfo, res interface{}) error {
	resp, err := c.Client.Do(req)
	if err != nil {
		return fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.Header.Get("Content-Encoding") == "gzip" {
		resp.Body, err = gzip.NewReader(resp.Body)
		if err != nil {
			return fmt.Errorf("gzip decode failed: %w", err)
		}
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("failed to read response body: %w", err)
	}

	return c.parseResponse(body, resp.StatusCode, res)
}

func (c *Client) parseResponse(body []byte, httpCode int, result interface{}) error {
	errResponse := &ErrorResponse{}
	isKOCode := httpCode < 200 || 299 < httpCode
	if isKOCode {
		errResponse.NetworkError = &HTTPError{
			Code:    httpCode,
			Message: fmt.Sprintf("Response body %s", string(body)),
		}
	}

	// some servers return a graphql error with a non OK http code, try anyway to parse the body
	if err := c.unmarshal(body, result); err != nil {
		var gqlErr *GqlErrorList
		if errors.As(err, &gqlErr) {
			errResponse.GqlErrors = &gqlErr.Errors
		} else if !isKOCode {
			return err
		}
	}

	if errResponse.HasErrors() {
		return errResponse
	}

	return nil
}

// response is a GraphQL layer response from a handler.
type response struct {
	Data   json.RawMessage `json:"data"`
	Errors json.RawMessage `json:"errors"`
}

func (c *Client) unmarshal(data []byte, res interface{}) error {
	resp := response{}
	if err := json.Unmarshal(data, &resp); err != nil {
		return fmt.Errorf("failed to decode data %s: %w", string(data), err)
	}

	var err error
	if resp.Errors != nil && len(resp.Errors) > 0 {
		// try to parse standard graphql error
		err = &GqlErrorList{}
		if e := json.Unmarshal(data, err); e != nil {
			return fmt.Errorf("faild to parse graphql errors. Response content %s - %w", string(data), e)
		}

		// if ParseDataWhenErrors is true, try to parse data as well
		if !c.ParseDataWhenErrors {
			return err
		}
	}

	if errData := graphqljson.UnmarshalData(resp.Data, res); errData != nil {
		// if ParseDataWhenErrors is true, and we failed to unmarshal data, return the actual error
		if c.ParseDataWhenErrors {
			return err
		}

		return fmt.Errorf("failed to decode data into response %s: %w", string(data), errData)
	}

	return err
}

func MarshalJSON(v interface{}) ([]byte, error) {
	if v == nil {
		return []byte("null"), nil // Directly return "null" for nil interface{}
	}

	val := reflect.ValueOf(v)
	if !val.IsValid() || (val.Kind() == reflect.Ptr && val.IsNil()) {
		return []byte("null"), nil // Return "null" for nil pointer or invalid reflect value
	}

	encoderFunc := getTypeEncoder(reflect.TypeOf(v))
	return encoderFunc(v)
}

// getTypeEncoder returns an appropriate encoder function for the provided type.
func getTypeEncoder(t reflect.Type) func(a any) ([]byte, error) {
	if t.Implements(reflect.TypeOf((*graphql.Marshaler)(nil)).Elem()) {
		return gqlMarshalerEncoder
	}

	switch t.Kind() {
	case reflect.Ptr:
		return newPtrEncoder(t)
	case reflect.Struct:
		return newStructEncoder(t)
	case reflect.Map:
		return newMapEncoder(t)
	case reflect.Slice:
		return newSliceEncoder(t)
	case reflect.Array:
		return newArrayEncoder(t)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return newIntEncoder()
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return newUintEncoder()
	case reflect.String:
		return newStringEncoder()
	case reflect.Bool:
		return newBoolEncoder()
	case reflect.Float32, reflect.Float64:
		return newFloatEncoder()
	case reflect.Interface:
		return newInterfaceEncoder()
	case reflect.Invalid, reflect.Complex64, reflect.Complex128, reflect.Chan, reflect.Func, reflect.UnsafePointer:
		panic(fmt.Sprintf("unsupported type: %s", t))
	default:
		panic(fmt.Sprintf("unsupported type: %s", t))
	}
}

func gqlMarshalerEncoder(v interface{}) ([]byte, error) {
	var buf bytes.Buffer
	if val, ok := v.(graphql.Marshaler); ok {
		val.MarshalGQL(&buf)
	} else {
		return nil, fmt.Errorf("failed to encode graphql.Marshaler: %v", v)
	}

	return buf.Bytes(), nil
}

func newBoolEncoder() func(interface{}) ([]byte, error) {
	return func(v interface{}) ([]byte, error) {
		if v, ok := v.(bool); ok {
			boolValue, err := json.Marshal(v)
			if err != nil {
				return nil, fmt.Errorf("failed to encode bool: %v", v)
			}
			return boolValue, nil
		} else {
			return nil, fmt.Errorf("failed to encode bool: %v", v)
		}
	}
}

func newIntEncoder() func(interface{}) ([]byte, error) {
	return func(v interface{}) ([]byte, error) {
		return []byte(fmt.Sprintf("%d", v)), nil
	}
}

func newUintEncoder() func(interface{}) ([]byte, error) {
	return func(v interface{}) ([]byte, error) {
		return []byte(fmt.Sprintf("%d", v)), nil
	}
}

func newFloatEncoder() func(interface{}) ([]byte, error) {
	return func(v interface{}) ([]byte, error) {
		return []byte(fmt.Sprintf("%f", v)), nil
	}
}

func newStringEncoder() func(interface{}) ([]byte, error) {
	return func(v interface{}) ([]byte, error) {
		stringValue, err := json.Marshal(v)
		if err != nil {
			return nil, fmt.Errorf("failed to encode string: %v", v)
		}

		return stringValue, nil
	}
}

type fieldInfo struct {
	name     string
	jsonName string
	typ      reflect.Type
}

func prepareFields(t reflect.Type) []fieldInfo {
	num := t.NumField()
	fields := make([]fieldInfo, 0, num)
	for i := 0; i < num; i++ {
		f := t.Field(i)
		if f.PkgPath != "" && !f.Anonymous { // Skip unexported fields unless they are embedded
			continue
		}
		jsonTag := f.Tag.Get("json")
		if jsonTag == "-" {
			continue // Skip fields explicitly marked to be ignored
		}
		jsonName := f.Name
		if jsonTag != "" {
			parts := strings.Split(jsonTag, ",")
			jsonName = parts[0] // Use the name specified in the JSON tag
		}
		fields = append(fields, fieldInfo{
			name:     f.Name,
			jsonName: jsonName,
			typ:      f.Type,
		})
	}

	return fields
}

func checkMarshalerFields(t reflect.Type) bool {
	switch t.Kind() {
	case reflect.Ptr:
		return checkMarshalerFields(t.Elem())

	case reflect.Struct:
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			if isMarshalerType(f.Type) {
				return true
			}
			// Recursively check for nested structs
			if checkMarshalerFields(f.Type) {
				return true
			}
		}

	case reflect.Map:
		// Check both key and value types for Marshaler implementation; usually, value type is what matters
		keyType, valueType := t.Key(), t.Elem()
		if isMarshalerType(valueType) || isMarshalerType(keyType) {
			return true
		}
		// Recursively check the map value type
		if checkMarshalerFields(valueType) {
			return true
		}

	case reflect.Slice, reflect.Array:
		// Recursively check the element type
		return checkMarshalerFields(t.Elem())
	case reflect.Interface, reflect.Invalid, reflect.Bool, reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return false
	default:
		return false
	}

	return false
}

func isMarshalerType(t reflect.Type) bool {
	if t.Implements(reflect.TypeOf((*graphql.Marshaler)(nil)).Elem()) {
		return true
	}
	if reflect.PtrTo(t).Implements(reflect.TypeOf((*graphql.Marshaler)(nil)).Elem()) {
		return true
	}
	return false
}

func newStructEncoder(t reflect.Type) func(interface{}) ([]byte, error) {
	fields := prepareFields(t)
	marshalerFieldExists := checkMarshalerFields(t)

	return func(v any) ([]byte, error) {
		// If no field implements the MarshalerGQL interface, use standard JSON marshaling
		if !marshalerFieldExists {
			return json.Marshal(v)
		}

		val := reflect.ValueOf(v)
		result := make(map[string]json.RawMessage)
		for _, field := range fields {
			fieldValue := val.FieldByName(field.name)
			if !fieldValue.IsValid() || (fieldValue.Kind() == reflect.Ptr && fieldValue.IsNil()) {
				continue // Skip invalid or nil pointers to avoid panics
			}
			encoder := getTypeEncoder(field.typ)
			encodedValue, err := encoder(fieldValue.Interface())
			if err != nil {
				return nil, err
			}
			result[field.jsonName] = encodedValue
		}
		return json.Marshal(result)
	}
}

func trimQuotes(s string) string {
	if len(s) > 1 && s[0] == '"' && s[len(s)-1] == '"' {
		return s[1 : len(s)-1]
	}

	return s
}

func newMapEncoder(t reflect.Type) func(interface{}) ([]byte, error) {
	keyEncoder := getTypeEncoder(t.Key())
	valueEncoder := getTypeEncoder(t.Elem())

	return func(v interface{}) ([]byte, error) {
		val := reflect.ValueOf(v)
		result := make(map[string]json.RawMessage)
		for _, key := range val.MapKeys() {
			encodedKey, err := keyEncoder(key.Interface())
			if err != nil {
				return nil, err
			}
			keyStr := string(encodedKey)
			keyStr = trimQuotes(keyStr)

			value := val.MapIndex(key)
			encodedValue, err := valueEncoder(value.Interface())
			if err != nil {
				return nil, err
			}
			result[keyStr] = encodedValue
		}

		return json.Marshal(result)
	}
}

func newSliceEncoder(t reflect.Type) func(interface{}) ([]byte, error) {
	elemEncoder := getTypeEncoder(t.Elem())
	return func(v interface{}) ([]byte, error) {
		val := reflect.ValueOf(v)
		result := make([]json.RawMessage, val.Len())
		for i := 0; i < val.Len(); i++ {
			encodedValue, err := elemEncoder(val.Index(i).Interface())
			if err != nil {
				return nil, err
			}
			result[i] = encodedValue
		}

		return json.Marshal(result)
	}
}

func newArrayEncoder(t reflect.Type) func(interface{}) ([]byte, error) {
	elemEncoder := getTypeEncoder(t.Elem())
	return func(v interface{}) ([]byte, error) {
		val := reflect.ValueOf(v)
		result := make([]json.RawMessage, val.Len())
		for i := 0; i < val.Len(); i++ {
			encodedValue, err := elemEncoder(val.Index(i).Interface())
			if err != nil {
				return nil, err
			}
			result[i] = encodedValue
		}

		return json.Marshal(result)
	}
}

func newPtrEncoder(t reflect.Type) func(interface{}) ([]byte, error) {
	if t.Elem().Kind() == reflect.Ptr {
		return newPtrEncoder(t.Elem())
	}
	elemEncoder := getTypeEncoder(t.Elem())
	return func(v interface{}) ([]byte, error) {
		val := reflect.ValueOf(v)
		if val.IsNil() {
			return []byte("null"), nil
		}

		return elemEncoder(val.Elem().Interface())
	}
}

func newInterfaceEncoder() func(interface{}) ([]byte, error) {
	return func(v interface{}) ([]byte, error) {
		if v == nil {
			return []byte("null"), nil
		}
		actualValue := reflect.ValueOf(v)
		if actualValue.Kind() == reflect.Interface && !actualValue.IsNil() {
			// Extract the element inside the interface value
			actualValue = actualValue.Elem()
		}
		if actualValue.IsValid() {
			actualType := actualValue.Type()
			encoder := getTypeEncoder(actualType)

			return encoder(actualValue.Interface())
		}

		return []byte("null"), nil
	}
}
