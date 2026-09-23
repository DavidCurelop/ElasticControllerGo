package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/feature/ec2/imds"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"
	cwtypes "github.com/aws/aws-sdk-go-v2/service/cloudwatch/types"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/elasticloadbalancingv2"
	elbtypes "github.com/aws/aws-sdk-go-v2/service/elasticloadbalancingv2/types"
)

type InstanceService struct {
	ec2Client *ec2.Client
}

type MetricService struct {
	cloudwatchClient *cloudwatch.Client
}

type elbService struct {
	elbClient *elasticloadbalancingv2.Client
}

type MetricRecord struct {
	ID    string
	Value float64
}

type instanceCreationInput struct {
	imageID          *string
	instanceType     types.InstanceType
	instanceTag      string
	instanceKeyName  string
	securityGroupIDs []string
}

type ControllerConfig struct {
	TargetGroupARN             string             `json:"TargetGroupARN"`
	InstanceAMI                string             `json:"InstanceAMI"`
	InstanceTag                string             `json:"InstanceTag"`
	InstanceType               types.InstanceType `json:"InstanceType"`
	ErrorCooldownSeconds       int                `json:"ErrorCooldownSeconds"`
	MaxInstances               int                `json:"MaxInstances"`
	MinInstances               int                `json:"MinInstances"`
	InstanceKey                string             `json:"InstanceKey"`
	SecurityGroupIDs           []string           `json:"SecurityGroupIDs"`
	LookbackWindowMinutes      int                `json:"LookbackWindowMinutes"`
	AVGCPUIncreaseThreshold    int                `json:"AVGCPUIncreaseThreshold"`
	AVGCPUDecreaseThreshold    int                `json:"AVGCPUDecreaseThreshold"`
	CPUChangeIncreaseThreshold int                `json:"CPUChangeIncreaseThreshold"`
	AVGNetInIncreaseThreshold  int                `json:"AVGNetInIncreaseThreshold"`
	AVGNetInDecreaseThreshold  int                `json:"AVGNetInDecreaseThreshold"`
}

func LoadControllerConfig(configFilePath string) (*ControllerConfig, error) {
	// TODO: Step 1 - Read file bytes with os.ReadFile(configFilePath)
	configJSON, err := os.ReadFile(configFilePath)
	// TODO: Step 2 - Guard against error with fmt.Errorf("reading config file %q: %w", ...)
	if err != nil {
		return DefaultControllerConfig(), fmt.Errorf("Error loading config file %v, loading default config %w", configFilePath, err)
	}

	var controllerConfig ControllerConfig
	// TODO: Step 3 - Unmarshal bytes into &controllerConfig with json.Unmarshal
	err = json.Unmarshal(configJSON, &controllerConfig)
	// TODO: Step 4 - Guard against unmarshal error with storytelling wrapping
	if err != nil {
		return DefaultControllerConfig(), fmt.Errorf("Error unmarshalling file %v, loading default config %w", configFilePath, err)
	}
	return &controllerConfig, nil
}

func DefaultControllerConfig() *ControllerConfig {
	return &ControllerConfig{
		TargetGroupARN: "arn:aws:elasticloadbalancing:us-east-1:046172315547:targetgroup/WebServerTG/60e063ee1bef4ce2",
		InstanceAMI: "ami-0f8a61b66d1accaee",

		InstanceTag:  "WebServer",
		InstanceType: types.InstanceTypeT2Micro,

		ErrorCooldownSeconds:       5,
		MaxInstances:               5,
		MinInstances:               1,
		InstanceKey:                "vockey",
		SecurityGroupIDs:           []string{"sg-0e41161d4476bb460"},
		LookbackWindowMinutes:      2,
		AVGCPUIncreaseThreshold:    70,
		AVGCPUDecreaseThreshold:    35,
		CPUChangeIncreaseThreshold: 10,
		AVGNetInIncreaseThreshold:  20,
		AVGNetInDecreaseThreshold:  5,
	}
}

func findMetricValue(records []cwtypes.MetricDataResult, targetID string, lookback int) (float64, time.Time, bool) {
	if len(records) == 0 || lookback < 0 {
		return 0.0, time.Time{}, false
	}
	for _, record := range records {
		if aws.ToString(record.Id) == targetID {
			if lookback >= len(record.Values) || lookback >= len(record.Timestamps) {
				return 0.0, time.Time{}, false
			}
			return record.Values[lookback], record.Timestamps[lookback], true
		}
	}
	return 0.0, time.Time{}, false
}

func main() {
	configAddress := flag.String("conf", "./config.json", "Controller config file")
	logAddress := flag.String("log", "./controller.log", "Controller log file")
	flag.Parse()
	logFile, err := os.OpenFile(*logAddress, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		log.Fatalf("opening controller log file: %v", err)
	}
	defer logFile.Close()
	log.SetOutput(io.MultiWriter(os.Stdout, logFile))

	var lastScaleTime time.Time

	// TODO: Step 1 - Load config via config.LoadDefaultConfig(ctx)
	ctx := context.Background()
	//awsConfig, configError := config.LoadDefaultConfig(ctx)
	awsConfig, configError := config.LoadDefaultConfig(ctx)
	// TODO: Step 2 - Check err; if err != nil, exit with log.Fatalf(...)
	if configError != nil {
		log.Fatalf("AWS config loading failed with error %v", configError)
	}

	if awsConfig.Region == "" {
		imdsClient := imds.NewFromConfig(awsConfig)
		regionOutput, err := imdsClient.GetRegion(ctx, &imds.GetRegionInput{})
		if err != nil {
			log.Fatalf("Couldnt identify current region %v", err)
		}
		awsConfig.Region = regionOutput.Region
	}

	controllerConfig, err := LoadControllerConfig(*configAddress)
	if err != nil {
		log.Printf("loading configuration: %v", err)
	}

	errorCooldown := time.Duration(controllerConfig.ErrorCooldownSeconds) * time.Second
	lookbackWindow := time.Duration(controllerConfig.LookbackWindowMinutes) * time.Minute

	instanceData := &instanceCreationInput{
		imageID:          aws.String(controllerConfig.InstanceAMI),
		instanceType:     controllerConfig.InstanceType,
		instanceTag:      controllerConfig.InstanceTag,
		instanceKeyName:  controllerConfig.InstanceKey,
		securityGroupIDs: controllerConfig.SecurityGroupIDs,
	}

	// Service creation and instantiation
	ec2Client := ec2.NewFromConfig(awsConfig)
	instanceService := &InstanceService{
		ec2Client: ec2Client,
	}

	elbClient := elasticloadbalancingv2.NewFromConfig(awsConfig)

	elbService := &elbService{
		elbClient: elbClient,
	}

	cwClient := cloudwatch.NewFromConfig(awsConfig)

	cwService := &MetricService{
		cloudwatchClient: cwClient,
	}

	for Loop := 1; true; Loop++ {
		log.Printf("\nLoop #%v started", Loop)

		//Get instances with the tag WebServer
		instances, err := instanceService.GetInstancesByTag(ctx, "Name", instanceData.instanceTag)
		log.Printf("Found %d matching instances", len(instances))

		if err != nil {
			log.Printf("Error retrieving instances %s", err)
			time.Sleep(errorCooldown)
			continue
		}

		if len(instances) == 0 {
			log.Printf("INCREASE_CAPACITY, No instances currently running")
			capIncreaseErr := INCREASE_CAPACITY(ctx, instanceService, elbService, instanceData, controllerConfig.TargetGroupARN)
			if capIncreaseErr != nil {
				log.Printf("Error while increasing capacity: %v", capIncreaseErr)
				time.Sleep(errorCooldown)
				continue
			}
			lastScaleTime = time.Now()
			time.Sleep(60 * time.Second)
			continue
		}
		//register running instances to TG

		for i, instance := range instances {
			instanceHealthOutput, err := elbService.GetTargetHealth(ctx, controllerConfig.TargetGroupARN, instance)
			if err != nil {
				continue
			}
			log.Printf("%v: Instance %v of type %v, has state: %v and TG state is: %v\n", i+1, aws.ToString(instance.InstanceId), instance.InstanceType, instance.State.Name, instanceHealthOutput.State)

			/*
				instanceTargetGroup := instanceHealthOutput.State

				if instance.State.Name == "running" && instanceTargetGroup == elbtypes.TargetHealthStateEnumUnused {
					elberr := elbService.RegisterTarget(ctx, tgARN, instance)
					if elberr != nil {
						log.Printf("Error assigning %v to TG: %s\n", aws.ToString(instance.InstanceId), elberr)
						time.Sleep(errorCooldown)
						continue
					}
					fmt.Printf("Successfully added %v to TG\n", aws.ToString(instance.InstanceId))
				}*/
		}

		if time.Since(lastScaleTime) < lookbackWindow {
			remaining := lookbackWindow - time.Since(lastScaleTime)
			log.Printf("MAINTAIN_CAPACITY, Reason: In cooldown (%.0fs remaining)", remaining.Seconds())
			time.Sleep(60 * time.Second)
			continue
		}

		//Get metrics for instances
		metrics, err := cwService.GetAllMetrics(ctx, instances, lookbackWindow)
		if err != nil {
			log.Printf("Error loading metrics %v", err)
			time.Sleep(errorCooldown)
			continue
		}

		avgCPU, cpuChangeDelta := getMetricaAVGandChange(metrics, "cpuQuery")
		avgNetIn, NetInChangeDelta := getMetricaAVGandChange(metrics, "networkInQuery")
		const bytesPerMegaByte = 1024 * 1024
		avgNetInMB := avgNetIn / bytesPerMegaByte
		netInChangeDeltaMB := NetInChangeDelta / bytesPerMegaByte

		log.Printf("Lookback window: %v, Period: 1min, Metrics:", lookbackWindow)
		log.Printf("AVG CPU: %v  CPU change: %v", avgCPU, cpuChangeDelta)
		log.Printf("AVG Net In (MB): %v  Net In change: %v", avgNetInMB, netInChangeDeltaMB)

		var increaseReason string
		capacityHeadroom := len(instances) < controllerConfig.MaxInstances
		switch {
		case avgCPU > float64(controllerConfig.AVGCPUIncreaseThreshold):
			increaseReason = fmt.Sprintf("Average CPU is > %v%%, currently: %v", controllerConfig.AVGCPUIncreaseThreshold, avgCPU)
		case cpuChangeDelta > float64(controllerConfig.CPUChangeIncreaseThreshold) && avgCPU > 50:
			increaseReason = fmt.Sprintf("Average CPU is > 50%%,currently: %v AND CPU usage change was: %v > %v", avgCPU, cpuChangeDelta, controllerConfig.CPUChangeIncreaseThreshold)
		case avgNetInMB > float64(controllerConfig.AVGNetInIncreaseThreshold):
			increaseReason = fmt.Sprintf("Average NetIn is > %vmb, currently: %v", controllerConfig.AVGNetInIncreaseThreshold, avgNetInMB)
		}

		if capacityHeadroom && increaseReason != "" {
			log.Printf("INCREASE_CAPACITY, %v", increaseReason)
			capIncreaseErr := INCREASE_CAPACITY(ctx, instanceService, elbService, instanceData, controllerConfig.TargetGroupARN)
			if capIncreaseErr != nil {
				log.Printf("Error while increasing capacity: %v", capIncreaseErr)
				time.Sleep(errorCooldown)
				continue
			}
			lastScaleTime = time.Now()
			time.Sleep(60 * time.Second)
			continue
		}

		var decreaseReason string
		capacityAboveMin := len(instances) > controllerConfig.MinInstances
		switch {
		case avgCPU < float64(controllerConfig.AVGCPUDecreaseThreshold) && avgNetInMB < float64(controllerConfig.AVGNetInDecreaseThreshold):
			decreaseReason = fmt.Sprintf("Average CPU is < %v%%, currently: %v AND Average NetIn is < %vmb, currently: %v", controllerConfig.AVGCPUDecreaseThreshold, avgCPU, controllerConfig.AVGNetInDecreaseThreshold, avgNetInMB)
		}

		if capacityAboveMin && decreaseReason != "" {
			log.Printf("REDUCE_CAPACITY, %v", decreaseReason)
			err := REDUCE_CAPACITY(ctx, instanceService, elbService, instances, controllerConfig.TargetGroupARN, 1)
			if err != nil {
				log.Printf("Error while reducing capacity: %v", err)
				time.Sleep(errorCooldown)
				continue
			}
			lastScaleTime = time.Now()
			time.Sleep(60 * time.Second)
			continue
		}
		var maintianReason string

		switch {
		case !capacityHeadroom && increaseReason != "":
			maintianReason = fmt.Sprintf("Scale-up desired (%s) but capped at max (%d)", increaseReason, controllerConfig.MaxInstances)
		case !capacityAboveMin && decreaseReason != "":
			maintianReason = fmt.Sprintf("Scale-down desired (%s) but protected at min (%d)", decreaseReason, controllerConfig.MinInstances)
		default:
			maintianReason = fmt.Sprintf("Metrics in steady state (CPU: %.1f%%, NetIn: %.2f MB/min)", avgCPU, avgNetInMB)
		}

		log.Printf("MAINTAIN_CAPACITY, Reason: %v", maintianReason)
		time.Sleep(60 * time.Second)
	}

}

func (s *InstanceService) LaunchInstance(ctx context.Context, instanceCreationInput instanceCreationInput) (types.Instance, error) {
	// TODO: Step 1 - Construct &ec2.RunInstancesInput with explicit, multi-line named fields:
	//       - ImageId (use aws.String)
	//       - InstanceType (the types.InstanceType passed in)
	//       - MinCount & MaxCount (use aws.Int32)

	rawUserDataScript := `#!/bin/bash
echo "<h1>Hello World from $(hostname -f)</h1>" | sudo tee /var/www/html/index.html
`
	encodedUserData := base64.StdEncoding.EncodeToString([]byte(rawUserDataScript))

	runInstanceInput := &ec2.RunInstancesInput{
		ImageId:          instanceCreationInput.imageID,
		InstanceType:     instanceCreationInput.instanceType,
		MinCount:         aws.Int32(1),
		MaxCount:         aws.Int32(1),
		KeyName:          &instanceCreationInput.instanceKeyName,
		SecurityGroupIds: instanceCreationInput.securityGroupIDs,
		Monitoring:       &types.RunInstancesMonitoringEnabled{Enabled: aws.Bool(true)},
		UserData:         &encodedUserData,
		TagSpecifications: []types.TagSpecification{
			{
				ResourceType: types.ResourceTypeInstance,
				Tags:         []types.Tag{{Key: aws.String("Name"), Value: &instanceCreationInput.instanceTag}},
			},
		},
	}

	// TODO: Step 2 - Call s.ec2Client.RunInstances(ctx, runInstancesInput)

	runInstancesOutput, err := s.ec2Client.RunInstances(ctx, runInstanceInput)

	// TODO: Step 3 - Check error immediately with early return ("Line of sight").
	//       Wrap the error using: fmt.Errorf("launching EC2 instance with AMI %q: %w", imageID, err)
	if err != nil {
		return types.Instance{}, fmt.Errorf("launching EC2 instance with AMI %q: %w", aws.ToString(instanceCreationInput.imageID), err)
	}

	if len(runInstancesOutput.Instances) == 0 {
		return types.Instance{}, fmt.Errorf("no %q instance created", aws.ToString(instanceCreationInput.imageID))
	}

	// TODO: Step 4 - Inspect the returned reservation output to extract and return the instance ID string.
	return runInstancesOutput.Instances[0], nil
}

func (s *InstanceService) GetInstancesByTag(ctx context.Context, tagName string, tagValue string) ([]types.Instance, error) {
	// TODO: Step 1 - Construct &ec2.DescribeInstancesInput with a types.Filter ("tag:" + tagName)
	describeInstancesInput := &ec2.DescribeInstancesInput{
		Filters: []types.Filter{{
			Name: aws.String("tag:" + tagName),
			Values: []string{
				*aws.String(tagValue),
			},
		}},
	}
	// TODO: Step 2 - Call s.ec2Client.DescribeInstances(ctx, describeInstancesInput)
	describeInstanceOutput, err := s.ec2Client.DescribeInstances(ctx, describeInstancesInput)
	// TODO: Step 3 - Handle error with early exit and storytelling fmt.Errorf wrapping
	if err != nil {
		return nil, fmt.Errorf("Something went wrong while looking for instances %w", err)
	}
	// TODO: Step 4 - Initialize a slice: var instanceIDs []string
	var instances []types.Instance
	// TODO: Step 5 - Range over Reservations and Instances, appending aws.ToString(inst.InstanceId)
	for _, reservtion := range describeInstanceOutput.Reservations {
		for _, instance := range reservtion.Instances {
			if instance.State.Name == types.InstanceStateNameRunning || instance.State.Name == types.InstanceStateNamePending {
				instances = append(instances, instance)
			}
		}
	}
	// TODO: Step 6 - Return instanceIDs and nil
	return instances, nil
}

func (s *InstanceService) TerminateInstances(ctx context.Context, instances []types.Instance) (*ec2.TerminateInstancesOutput, error) {
	var instanceIds []string
	for _, instance := range instances {
		instanceIds = append(instanceIds, *instance.InstanceId)
	}

	terminateInput := &ec2.TerminateInstancesInput{
		InstanceIds: instanceIds,
	}
	TerminateOutput, err := s.ec2Client.TerminateInstances(ctx, terminateInput)

	if err != nil {
		return nil, fmt.Errorf("%w", err)
	}

	for _, terminatedInstance := range TerminateOutput.TerminatingInstances {
		fmt.Printf("Succesfully terminated Instance: %s Status: %s\n", aws.ToString(terminatedInstance.InstanceId), terminatedInstance.CurrentState.Name)
	}

	return TerminateOutput, nil
}

func (s *MetricService) GetAllMetrics(ctx context.Context, instances []types.Instance, lookbackWindow time.Duration) ([][]cwtypes.MetricDataResult, error) {
	var metrics [][]cwtypes.MetricDataResult
	for _, instance := range instances {
		metricResult, err := s.GetInstanceMetrics(ctx, aws.ToString(instance.InstanceId), lookbackWindow)
		if err != nil {
			return nil, fmt.Errorf("Error getting instance metrics: %w", err)
		}

		metrics = append(metrics, metricResult)
	}
	return metrics, nil
}

func (s *MetricService) GetInstanceMetrics(ctx context.Context, instanceID string, lookbackWindow time.Duration) ([]cwtypes.MetricDataResult, error) {
	queryEndTime := time.Now()
	queryStartTime := queryEndTime.Add(-lookbackWindow)
	// TODO: Step 1 - Construct &cloudwatch.GetMetricDataInput with explicit multi-line fields:
	//       - StartTime: aws.Time(queryStartTime)
	//       - EndTime:   aws.Time(queryEndTime)
	//       - MetricDataQueries: []types.MetricDataQuery{ ... }
	//         Each query needs:
	//           - Id: (a unique query identifier string, e.g., "cpuQuery")
	//           - MetricStat: Metric (Namespace, MetricName, Dimensions), Period, Stat
	getmetricsInput := cloudwatch.GetMetricDataInput{
		StartTime: aws.Time(queryStartTime),
		EndTime:   aws.Time(queryEndTime),
		ScanBy:    cwtypes.ScanByTimestampDescending,
		MetricDataQueries: []cwtypes.MetricDataQuery{
			{Id: aws.String("cpuQuery"),
				MetricStat: &cwtypes.MetricStat{
					Period: aws.Int32(60),
					Stat:   aws.String("Average"),
					Metric: &cwtypes.Metric{
						Namespace:  aws.String("AWS/EC2"),
						MetricName: aws.String("CPUUtilization"),
						Dimensions: []cwtypes.Dimension{{
							Name:  aws.String("InstanceId"),
							Value: aws.String(instanceID)},
						},
					},
				}},
			{Id: aws.String("networkInQuery"),
				MetricStat: &cwtypes.MetricStat{
					Period: aws.Int32(60),
					Stat:   aws.String("Average"),
					Metric: &cwtypes.Metric{
						Namespace:  aws.String("AWS/EC2"),
						MetricName: aws.String("NetworkIn"),
						Dimensions: []cwtypes.Dimension{{
							Name:  aws.String("InstanceId"),
							Value: aws.String(instanceID)},
						},
					},
				}},
		},
	}

	// TODO: Step 2 - Call s.cloudwatchClient.GetMetricData(ctx, metricDataInput)
	getmetricsOutput, err := s.cloudwatchClient.GetMetricData(ctx, &getmetricsInput)
	// TODO: Step 3 - Apply "Line of sight" error check and storytelling wrapping:
	//       fmt.Errorf("querying CloudWatch CPU metrics for instance %q: %w", instanceID, err)
	if err != nil {
		return nil, fmt.Errorf("querying CloudWatch metrics for instance %q: %w", instanceID, err)
	}
	// TODO: Step 4 - Inspect the returned results. Handle the "no data yet" case safely.
	//       Return the most recent data point value, or a sentinel/zero value if empty.
	return getmetricsOutput.MetricDataResults, nil
}

func (s *elbService) RegisterTarget(ctx context.Context, targetGroupARN string, instance types.Instance) error {
	// TODO: Step 1 - Build &elasticloadbalancingv2.RegisterTargetsInput
	registerTargetInput := &elasticloadbalancingv2.RegisterTargetsInput{
		TargetGroupArn: aws.String(targetGroupARN),
		Targets: []elbtypes.TargetDescription{{
			Id:               aws.String(aws.ToString(instance.InstanceId)),
			AvailabilityZone: nil,
		}},
	}
	// TODO: Step 2 - Call s.elbClient.RegisterTargets(ctx, registerTargetsInput)
	_, err := s.elbClient.RegisterTargets(ctx, registerTargetInput)
	// TODO: Step 3 - Line of sight error handling with storytelling wrapping:
	//       fmt.Errorf("registering instance %q to target group %q: %w", instanceID, targetGroupARN, err)
	if err != nil {
		return fmt.Errorf("registering instance %q to target group %q: %w", aws.ToString(instance.InstanceId), targetGroupARN, err)
	}
	return err
}

func (s *elbService) deRegisterTarget(ctx context.Context, targetGroupARN string, instance types.Instance) error {
	// TODO: Step 1 - Build &elasticloadbalancingv2.RegisterTargetsInput
	deRegisterTargetInput := &elasticloadbalancingv2.DeregisterTargetsInput{
		TargetGroupArn: aws.String(targetGroupARN),
		Targets: []elbtypes.TargetDescription{{
			Id:               instance.InstanceId,
			AvailabilityZone: nil,
		}},
	}
	// TODO: Step 2 - Call s.elbClient.RegisterTargets(ctx, registerTargetsInput)
	_, err := s.elbClient.DeregisterTargets(ctx, deRegisterTargetInput)
	// TODO: Step 3 - Line of sight error handling with storytelling wrapping:
	//       fmt.Errorf("registering instance %q to target group %q: %w", instanceID, targetGroupARN, err)
	if err != nil {
		return fmt.Errorf("deregistering instance %q from target group %q: %w", *instance.InstanceId, targetGroupARN, err)
	}
	return err
}

func (s *elbService) GetTargetHealth(ctx context.Context, targetGroupARN string, instance types.Instance) (elbtypes.TargetHealth, error) {
	// Step 1 - Construct DescribeTargetHealthInput with TargetGroupArn & Targets
	THInput := &elasticloadbalancingv2.DescribeTargetHealthInput{
		TargetGroupArn: aws.String(targetGroupARN),
		Targets: []elbtypes.TargetDescription{
			{
				Id: instance.InstanceId,
			},
		},
	}

	// Step 2 - Call s.elbClient.DescribeTargetHealth(ctx, THInput)
	THOutput, err := s.elbClient.DescribeTargetHealth(ctx, THInput)

	// Step 3 - Line-of-sight error check, wrapped with context
	if err != nil {
		return elbtypes.TargetHealth{}, fmt.Errorf("describing target health for instance %s in target group %s: %w",
			aws.ToString(instance.InstanceId), targetGroupARN, err)
	}

	// Step 4 - Defensively inspect TargetHealthDescriptions
	if len(THOutput.TargetHealthDescriptions) == 0 {
		return elbtypes.TargetHealth{}, fmt.Errorf("no target health descriptions returned for instance %s in target group %s",
			aws.ToString(instance.InstanceId), targetGroupARN)
	}

	thd := THOutput.TargetHealthDescriptions[0]
	if thd.TargetHealth == nil {
		return elbtypes.TargetHealth{}, fmt.Errorf("target health is nil for instance %s in target group %s",
			aws.ToString(instance.InstanceId), targetGroupARN)
	}

	// Step 5 - Return the TargetHealth struct (its State field holds the enum) and nil
	return *thd.TargetHealth, nil
}

func (s *elbService) allInstancesHealthy(ctx context.Context, targetGroupARN string, instances []types.Instance) (bool, error) {
	if len(instances) == 0 {
		return false, fmt.Errorf("Instances must have at least one instance")
	}

	for _, instance := range instances {
		health, err := s.GetTargetHealth(ctx, targetGroupARN, instance)

		if err != nil {
			return false, fmt.Errorf("There was an error with instance %v in %v TG:%w", aws.ToString(instance.InstanceId), targetGroupARN, err)
		}

		if health.State != elbtypes.TargetHealthStateEnumHealthy && health.State != elbtypes.TargetHealthStateEnumInitial {
			return false, nil
		}
	}
	return true, nil
}

func getMetricaAVGandChange(metrics [][]cwtypes.MetricDataResult, metricID string) (float64, float64) {
	var currentCPUsSum float64
	var previousCPUsSum float64

	if len(metrics) == 0 {
		return 0.0, 0.0
	}

	for _, metric := range metrics {
		currCPU, _, metricFound := findMetricValue(metric, metricID, 0)
		if metricFound {
			currentCPUsSum = currentCPUsSum + currCPU
		}
		prevCPU, _, metricFound := findMetricValue(metric, metricID, 1)
		if metricFound {
			previousCPUsSum = previousCPUsSum + prevCPU
		}
	}
	avgCurrentCPU := currentCPUsSum / float64(len(metrics))
	avgPreviousCPU := previousCPUsSum / float64(len(metrics))

	cpuDeltaChange := avgCurrentCPU - avgPreviousCPU

	return avgCurrentCPU, cpuDeltaChange
}

func (s *elbService) WaitForTargetStateHealth(ctx context.Context, targetGroupARN string, instance types.Instance, desiredState elbtypes.TargetHealthStateEnum) error {
	for {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("There was an error with the context %w", err)
		}
		// TODO: Step 2 - Call s.GetTargetHealth(ctx, targetGroupARN, instance)
		health, err := s.GetTargetHealth(ctx, targetGroupARN, instance)
		if err != nil {
			return fmt.Errorf("There was an error getting target %v health %w", aws.ToString(instance.InstanceId), err)
		}

		// TODO: Step 3 - If health.State == desiredState, return nil (success!)
		if health.State == desiredState {
			return nil
		}
		// TODO: Step 1 - Define a polling loop with an interval (e.g., time.Sleep(5 * time.Second))
		time.Sleep(5 * time.Second)
	}
}

// IsInstanceRunning queries EC2 to check whether an instance is in the "running" state.
func (s *InstanceService) IsInstanceRunning(ctx context.Context, instanceID string) (bool, error) {
	// Step 1: Explicit struct initialization targeting the instance ID.
	// IncludeAllInstances ensures EC2 returns status even when the instance is pending or stopped.
	describeInput := &ec2.DescribeInstanceStatusInput{
		InstanceIds:         []string{instanceID},
		IncludeAllInstances: aws.Bool(true),
	}

	// Step 2: Query the EC2 API via the injected ec2Client.
	describeOutput, err := s.ec2Client.DescribeInstanceStatus(ctx, describeInput)
	if err != nil {
		return false, fmt.Errorf("describing instance status for %q: %w", instanceID, err)
	}

	// Step 3: If no status records are returned, the instance is not yet registered in EC2 status.
	if len(describeOutput.InstanceStatuses) == 0 {
		return false, nil
	}

	// Step 4: Defensively check pointer safety before dereferencing nested struct fields.
	instanceStatus := describeOutput.InstanceStatuses[0]
	if instanceStatus.InstanceState == nil {
		return false, fmt.Errorf("instance state was nil for %q", instanceID)
	}

	// Step 5: Evaluate whether the lifecycle state matches running.
	isRunning := instanceStatus.InstanceState.Name == types.InstanceStateNameRunning
	return isRunning, nil
}

// WaitForInstanceRunning polls EC2 until the instance reaches the "running" state or ctx is cancelled.
func (s *InstanceService) WaitForInstanceRunning(ctx context.Context, instanceID string) error {
	pollInterval := 5 * time.Second
	for {
		// Step 1: Guard against context timeouts or cancellations.
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("context cancelled while waiting for instance %q to run: %w", instanceID, err)
		}

		// Step 2: Poll instance status using IsInstanceRunning.
		isRunning, err := s.IsInstanceRunning(ctx, instanceID)
		if err != nil {
			return fmt.Errorf("polling running state for instance %q: %w", instanceID, err)
		}

		// Step 3: Happy path exit when desired running state is achieved.
		if isRunning {
			return nil
		}

		// Step 4: Wait for the next poll interval before checking again.
		time.Sleep(pollInterval)
	}
}

func INCREASE_CAPACITY(ctx context.Context, instanceService *InstanceService, elbService *elbService, instanceData *instanceCreationInput, tgARN string) error {
	instance, err := instanceService.LaunchInstance(ctx, *instanceData)
	if err != nil {
		return fmt.Errorf("Error starting instance %v %w. Trying again", aws.ToString(instance.InstanceId), err)
	}

	log.Printf("Started instance %v succesfully", aws.ToString(instance.InstanceId))

	time.Sleep(5 * time.Second)

	errWait := instanceService.WaitForInstanceRunning(ctx, aws.ToString(instance.InstanceId))
	if errWait != nil {
		return fmt.Errorf("Error waiting for instance %v to reach running state: %v", aws.ToString(instance.InstanceId), errWait)

	}
	log.Printf("Instance %v is %v", aws.ToString(instance.InstanceId), instance.State.Name)

	err = elbService.RegisterTarget(ctx, tgARN, instance)

	if err != nil {
		return fmt.Errorf("Error assigning %v to TG: %s", aws.ToString(instance.InstanceId), err)
	}

	fmt.Printf("Successfully added %v to TG\n", aws.ToString(instance.InstanceId))

	err = elbService.WaitForTargetStateHealth(ctx, tgARN, instance, elbtypes.TargetHealthStateEnumHealthy)
	if err != nil {
		return fmt.Errorf("Error waiting for instance %v health %v", aws.ToString(instance.InstanceId), err)
	}

	log.Printf("Instance %v is now running and registered to TG %v", aws.ToString(instance.InstanceId), tgARN)

	return nil
}

func REDUCE_CAPACITY(ctx context.Context, instanceService *InstanceService, elbService *elbService, instances []types.Instance, tgARN string, amount int) error {
	var instancesToTerminate []types.Instance
	var instancesToTerminateID []string
	for i := 0; i < amount; i++ {
		instancesToTerminate = append(instancesToTerminate, instances[i])
		instancesToTerminateID = append(instancesToTerminateID, *instances[i].InstanceId)
		err := elbService.deRegisterTarget(ctx, tgARN, instancesToTerminate[i])
		if err != nil {
			return fmt.Errorf("Error deregistering %v from %v", instancesToTerminate[i], tgARN)

		}
		log.Printf("Deregistered instance with ID: %v from %v successfully!", aws.ToString(instancesToTerminate[i].InstanceId), tgARN)

	}

	elbService.WaitForTargetStateHealth(ctx, tgARN, instancesToTerminate[len(instancesToTerminate)-1], elbtypes.TargetHealthStateEnumUnused)

	instanceInfo, err := instanceService.TerminateInstances(ctx, instancesToTerminate)
	if err != nil {
		return fmt.Errorf("Error terminating instance %w", err)

	}
	describeInstancesInput := &ec2.DescribeInstancesInput{
		InstanceIds: instancesToTerminateID,
	}

	waiter := ec2.NewInstanceTerminatedWaiter(instanceService.ec2Client)

	err = waiter.Wait(ctx, describeInstancesInput, 3*time.Minute)
	if err != nil {
		return fmt.Errorf("Error waiting for instance to terminate %w", err)
	}

	for _, instanceState := range instanceInfo.TerminatingInstances {
		log.Printf("Terminated instance with ID: %v", aws.ToString(instanceState.InstanceId))
	}
	return nil
}
