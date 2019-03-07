package bgc

import (
	"fmt"
	"github.com/viant/dsc"
	"github.com/viant/toolbox"
	"github.com/viant/toolbox/data"
	"golang.org/x/net/context"
	"google.golang.org/api/bigquery/v2"
	"google.golang.org/api/googleapi"
	"io"
	"strings"
	"sync"
	"time"
)

const (
	InsertMethodStream       = "stream"
	InsertMethodLoad         = "load"
	InsertWaitTimeoutInMsKey = "insertWaitTimeoutInMs"
	StreamBatchCount         = "streamBatchCount" //can not be more than 10000
	jsonFormat               = "NEWLINE_DELIMITED_JSON"
	createIfNeeded           = "CREATE_IF_NEEDED"
	writeAppend              = "WRITE_APPEND"
)

//InsertTask represents insert streaming task.
type InsertTask struct {
	tableDescriptor   *dsc.TableDescriptor
	service           *bigquery.Service
	context           context.Context
	projectID         string
	datasetID         string
	waitForCompletion bool
	manager           dsc.Manager
	insertMethod      string
	columns           map[string]dsc.Column
}

//InsertSingle streams single records into big query.
func (it *InsertTask) InsertSingle(record map[string]interface{}) error {
	_, err := it.InsertAll([]map[string]interface{}{record})
	return err
}

func (it *InsertTask) insertID(record map[string]interface{}) string {
	pkValue := ""
	for _, pkColumn := range it.tableDescriptor.PkColumns {
		pkValue = pkValue + toolbox.AsString(record[pkColumn])
	}
	return pkValue
}

//normalizeValue rewrites data structure and remove nil values,
func normalizeValue(value interface{}) (interface{}, bool) {
	if value == nil {
		return nil, false
	}
	if val, ok := value.(interface{}); !ok || val == nil {
		return nil, false
	}
	value = toolbox.DereferenceValue(value)
	switch val := value.(type) {
	case string, int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, bool, float64, float32:
		return val, true
	}

	if toolbox.IsTime(value) {
		ts := toolbox.AsTime(value, "")
		return value, ts != nil
	} else if toolbox.IsStruct(value) {
		return normalizeValue(toolbox.AsMap(value))
	} else if toolbox.IsMap(value) {
		aMap := toolbox.AsMap(value)
		for k, v := range aMap {
			val, ok := normalizeValue(v)
			if !ok {
				delete(aMap, k)
				continue
			}
			aMap[k] = val
		}
		return aMap, len(aMap) > 0

	} else if toolbox.IsSlice(value) {
		aSlice := toolbox.AsSlice(value)
		newSlice := []interface{}{}
		for _, item := range aSlice {
			if val, ok := normalizeValue(item); ok {
				newSlice = append(newSlice, val)
			}
		}
		return newSlice, len(newSlice) > 0
	}
	return value, true
}

func (it *InsertTask) asJSONMap(record interface{}) map[string]bigquery.JsonValue {
	var jsonValues = make(map[string]bigquery.JsonValue)
	for k, v := range toolbox.AsMap(record) {
		val, ok := normalizeValue(v)
		if !ok {
			continue
		}
		if column, ok := it.columns[k]; ok {
			switch strings.ToLower(column.DatabaseTypeName()) {
			case "boolean":
				val = toolbox.AsBoolean(val)
			case "float":
				val = toolbox.AsFloat(val)
			}
		}
		jsonValues[k] = val
	}
	return jsonValues
}

func (it *InsertTask) asMap(record interface{}) map[string]interface{} {
	var jsonValues = make(map[string]interface{})
	for k, v := range toolbox.AsMap(record) {
		val, ok := normalizeValue(v)
		if !ok {
			continue
		}
		if column, ok := it.columns[k]; ok {
			switch strings.ToLower(column.DatabaseTypeName()) {
			case "boolean":
				val = toolbox.AsBoolean(val)
			case "float":
				val = toolbox.AsFloat(val)
			}
		}
		jsonValues[k] = val
	}
	return jsonValues
}

func buildRecord(record map[string]interface{}) map[string]interface{} {
	result := data.NewMap()
	for k, v := range toolbox.AsMap(record) {
		result.SetValue(k, v)
	}
	return toolbox.DeleteEmptyKeys(result)
}

func (it *InsertTask) buildLoadData(data interface{}) (io.Reader, int, error) {
	compressed := NewCompressed(nil)
	ranger, ok := data.(toolbox.Ranger)

	var count = 0
	if ok {
		if err := ranger.Range(func(item interface{}) (bool, error) {
			count++
			return true, compressed.Append(it.asMap(item))
		}); err != nil {
			return nil, 0, err
		}
	} else if toolbox.IsSlice(data) {
		for _, item := range toolbox.AsSlice(data) {
			if err := compressed.Append(it.asMap(item)); err != nil {
				return nil, 0, err
			}
			count++
		}
	} else {
		return nil, 0, fmt.Errorf("unsupported type: %T\n", data)
	}
	reader, err := compressed.GetAndClose()
	return reader, count, err
}

func (it *InsertTask) normalizeDataType(record map[string]interface{}) {

}

//InsertAll streams all records into big query, returns number records streamed or error.
func (it *InsertTask) LoadAll(data interface{}) (int, error) {
	mediaReader, count, err := it.buildLoadData(data)
	if err != nil || count == 0 {
		return count, err
	}

	if err = it.Insert(mediaReader); err != nil {
		return 0, err
	}
	return count, err
}

//InsertAll streams all records into big query, returns number records streamed or error.
func (it *InsertTask) Insert(reader io.Reader) error {
	req := &bigquery.Job{
		Configuration: &bigquery.JobConfiguration{
			Load: &bigquery.JobConfigurationLoad{
				SourceFormat: jsonFormat,
				DestinationTable: &bigquery.TableReference{
					ProjectId: it.projectID,
					DatasetId: it.datasetID,
					TableId:   it.tableDescriptor.Table,
				},
				CreateDisposition: createIfNeeded,
				WriteDisposition:  writeAppend,
			},
		},
	}
	call := it.service.Jobs.Insert(it.projectID, req)
	call = call.Media(reader, googleapi.ContentType("application/octet-stream"))
	job, err := call.Do()
	if err != nil {
		jobJSON, _ := toolbox.AsIndentJSONText(req)
		return fmt.Errorf("failed to submit insert job: %v, %v", jobJSON, err)
	}
	insertWaitTimeMs := it.manager.Config().GetInt(InsertWaitTimeoutInMsKey, 20000)
	_, err = waitForJobCompletion(it.service, it.context, it.projectID, job.JobReference.JobId, insertWaitTimeMs)
	return err
}

//InsertAll streams all records into big query, returns number records streamed or error.
func (it *InsertTask) StreamAll(data interface{}) (int, error) {
	records := toolbox.AsSlice(data)

	estimatedRows := len(records)
	tableRequest := it.service.Tables.Get(it.projectID, it.datasetID, it.tableDescriptor.Table)
	if table, err := tableRequest.Context(it.context).Do(); err == nil {
		if table.StreamingBuffer != nil {
			estimatedRows += int(table.StreamingBuffer.EstimatedRows)
		}
	}

	streamBatchCount := it.manager.Config().GetInt(StreamBatchCount, 9999)
	i := 0
	batchCount := 0
	streamRowCount := 0
	var err error
	var waitGroup = sync.WaitGroup{}
	waitGroup.Add(1)

	for i < len(records) {
		var rows = make([]*bigquery.TableDataInsertAllRequestRows, 0)
		for ; i < len(records); i++ {
			record := buildRecord(toolbox.AsMap(records[i]))
			rows = append(rows, &bigquery.TableDataInsertAllRequestRows{InsertId: it.insertID(record), Json: it.asJSONMap(record)})
			if len(rows) >= streamBatchCount {
				break
			}
		}
		if len(rows) == 0 {
			break
		}
		streamRowCount += len(rows)
		go func(batchCount int, rows []*bigquery.TableDataInsertAllRequestRows) {
			if batchCount == 0 {
				defer waitGroup.Done()
			}
			insertRequest := &bigquery.TableDataInsertAllRequest{}
			insertRequest.Rows = rows
			requestCall := it.service.Tabledata.InsertAll(it.projectID, it.datasetID, it.tableDescriptor.Table, insertRequest)
			response, e := requestCall.Context(it.context).Do()
			if e != nil {
				err = e
				return
			}
			if len(response.InsertErrors) > 0 {
				var messages = make([]string, 0)
				for _, insertError := range response.InsertErrors {
					if len(insertError.Errors) > 0 {
						info, _ := toolbox.AsJSONText(insertError.Errors[0])
						messages = append(messages, info)
						break
					}
				}
				if len(messages) > 0 {
					err = fmt.Errorf("%s", strings.Join(messages, ","))
				}
				if err == nil {
					err = fmt.Errorf("%v", response.InsertErrors[0])
				}
			}
		}(batchCount, rows)
		batchCount++
	}
	waitGroup.Wait() //wait for the first batch only
	if err != nil {
		return 0, err
	}
	//for testing purposes waits till data gets available in buffer, to supress this wait sets config.params.insertWaitTimeoutInMs=-1
	errCheck := it.waitForEmptyStreamingBuffer(estimatedRows)
	if err == nil {
		err = errCheck
	}
	return streamRowCount, err
}

func (it *InsertTask) waitForEmptyStreamingBuffer(estimatedRows int) error {
	insertWaitTimeMs := it.manager.Config().GetInt(InsertWaitTimeoutInMsKey, 60000)
	if insertWaitTimeMs <= 0 {
		return nil
	}
	waitSoFarMs := 0
	for i := 0; ; i++ {
		tableRequest := it.service.Tables.Get(it.projectID, it.datasetID, it.tableDescriptor.Table)
		table, err := tableRequest.Context(it.context).Do()
		if err != nil {
			return err
		}
		if table.StreamingBuffer == nil || table.StreamingBuffer.EstimatedRows == 0 || int(table.StreamingBuffer.EstimatedRows) >= estimatedRows {
			break
		}
		time.Sleep(time.Millisecond * time.Duration(tickInterval*(1+i%20)))
		waitSoFarMs += tickInterval
		if waitSoFarMs > insertWaitTimeMs {
			break
		}
	}
	return nil
}

//InsertAll streams or load all records into big query, returns number records streamed or error.
func (it *InsertTask) InsertAll(data interface{}) (int, error) {
	var count int
	var err error
	var retrySleepMs = 2000
	for i := 0; i < 3; i++ {
		count, err = it.insertAll(data)
		if err != nil && strings.Contains(err.Error(), "Error 503") {
			time.Sleep(time.Duration(retrySleepMs*(1+i)) * time.Millisecond)
			continue
		}
		break
	}
	return count, err
}

//InsertAll streams all records into big query, returns number records streamed or error.
func (it *InsertTask) insertAll(records interface{}) (int, error) {
	if it.insertMethod == InsertMethodStream {
		return it.StreamAll(records)
	}
	return it.LoadAll(records)
}

//NewInsertTask creates a new streaming insert task, it takes manager, table descript with schema, waitForCompletion flag with time duration.
func NewInsertTask(manager dsc.Manager, table *dsc.TableDescriptor, waitForCompletion bool) (*InsertTask, error) {
	config := manager.Config()
	service, ctx, err := GetServiceAndContextForManager(manager)
	if err != nil {
		return nil, err
	}
	insertMethod := config.GetString(fmt.Sprintf("%v.insertMethod", table.Table), InsertMethodLoad)
	dialect := dialect{}

	datastore, _ := dialect.GetCurrentDatastore(manager)

	var columns = make(map[string]dsc.Column)

	if dscColumns, err := dialect.GetColumns(manager, datastore, table.Table); err == nil {
		for _, column := range dscColumns {
			columns[column.Name()] = column
		}
	}
	return &InsertTask{
		tableDescriptor:   table,
		service:           service,
		context:           ctx,
		manager:           manager,
		insertMethod:      insertMethod,
		waitForCompletion: waitForCompletion,
		projectID:         config.Get(ProjectIDKey),
		datasetID:         config.Get(DataSetIDKey),
		columns:           columns,
	}, nil
}
