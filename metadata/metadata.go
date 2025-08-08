package metadata

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/pkg/errors"
)

const metaDataApi = "http://100.100.100.200/latest/meta-data/"
const userDataFile = "user_data.json"

var metaDataClient *http.Client

func init() {
	fmt.Println("Initializing HTTP client in init()...")
	metaDataClient = &http.Client{
		Timeout: 10 * time.Second,
	}
}

func ReadUserData() (map[string]string, error) {
	resp, err := metaDataClient.Get("http://34.124.242.198:8882/api/config")
	if err != nil {
		return nil, err
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	var res map[string]string
	err = json.Unmarshal(data, &res)
	return res, err
}

func get(url string) (string, error) {
	resp, err := metaDataClient.Get(url)
	if err != nil {
		return "", errors.Wrap(err, "failed to request metadata server")
	}

	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", errors.Wrap(fmt.Errorf("received non-200 status code: %d", resp.StatusCode), "")
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", errors.Wrap(err, "failed to read response body")
	}

	return string(body), nil
}

func InstanceId() (string, error) {
	url := metaDataApi + "instance-id"
	return get(url)
}

func RegionId() (string, error) {
	url := metaDataApi + "region-id"
	return get(url)
}
