package main

import (
	"context"
	"encoding/base64"
	"fmt"
	"log"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
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
	tgARN := "arn:aws:elasticloadbalancing:us-east-1:046172315547:targetgroup/WebServerTG/60e063ee1bef4ce2"
	//instanceAMI := "ami-0f8a61b66d1accaee"
	instanceAMI := "ami-098fa3be973dd6b19"

	instanceTag := "WebServer"
	instaceType := types.InstanceTypeT2Micro
	launchCooldown := 2 * time.Minute
	errorCooldown := 5 * time.Second
	maxInstances := 5
	minInstances := 1
	instnaceKey := "vockey"
	sgID := []string{"sg-0e41161d4476bb460"}

	// TODO: Step 1 - Load config via config.LoadDefaultConfig(ctx)
	ctx := context.Background()
	//awsConfig, configError := config.LoadDefaultConfig(ctx)
	awsConfig, configError := config.LoadDefaultConfig(ctx)
	// TODO: Step 2 - Check err; if err != nil, exit with log.Fatalf(...)
	if configError != nil {
		log.Fatalf("AWS config loading failed with error %v", configError)
	}

	instanceData := &instanceCreationInput{
		imageID:          aws.String(instanceAMI),
		instanceType:     instaceType,
		instanceTag:      instanceTag,
		instanceKeyName:  instnaceKey,
		securityGroupIDs: sgID,
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
		instances, err := instanceService.GetInstancesByTag(ctx, "Name", instanceTag)
		log.Printf("Found %d matching instances", len(instances))

		if err != nil {
			log.Printf("Error retrieving instances %s", err)
			time.Sleep(errorCooldown)
			continue
		}

		if len(instances) == 0 {
			log.Printf("No instances found, starting instance and restarting")
			instanceID, err := instanceService.LaunchInstance(ctx, *instanceData)
			if err != nil {
				log.Printf("Error starting instance %v %v", instanceID, err)
				continue
			}

			log.Printf("Started instance %v succesfully", instanceID)

			time.Sleep(launchCooldown)
			continue
		}
		//register running instances to TG
		for i, instance := range instances {
			fmt.Printf("%v: Instance %v of type %v and has state %v\n", i, aws.ToString(instance.InstanceId), instance.InstanceType, *aws.String(string(instance.State.Name)))
			instanceHealthOutput, err := elbService.GetTargetHealth(ctx, tgARN, instance)
			if err != nil {
				continue
			}

			instanceTargetGroup := instanceHealthOutput.State

			if instance.State.Name == "running" && instanceTargetGroup == elbtypes.TargetHealthStateEnumUnused {
				elberr := elbService.RegisterTarget(ctx, tgARN, instance)
				if elberr != nil {
					log.Printf("Error assigning %v to TG: %s\n", aws.ToString(instance.InstanceId), elberr)
					time.Sleep(errorCooldown)
					continue
				}
				fmt.Printf("Successfully added %v to TG\n", aws.ToString(instance.InstanceId))
			}
		}

		//Get metrics for instances
		metrics, err := cwService.GetAllMetrics(ctx, instances, 2*time.Minute)
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

		log.Printf("AVG CPU: %v  CPU change: %v", avgCPU, cpuChangeDelta)
		log.Printf("AVG Net In (MB): %v  Net In change: %v", avgNetInMB, netInChangeDeltaMB)

		if ((avgCPU > 70 || (cpuChangeDelta > 10 && avgCPU > 50)) || avgNetInMB > 10) && len(instances) < maxInstances {
			instanceID, err := instanceService.LaunchInstance(ctx, *instanceData)
			if err != nil {
				log.Printf("Error starting instance %v %v", instanceID, err)
				continue
			}
			log.Printf("Started instance with ID: %v", instanceID)
			time.Sleep(launchCooldown)
			continue
		}

		if avgCPU < 35 && len(instances) > minInstances && avgNetInMB < 5 {
			var instancesToTerminate []string
			instancesToTerminate = append(instancesToTerminate, aws.ToString(instances[0].InstanceId))
			err := elbService.deRegisterTarget(ctx, tgARN, instancesToTerminate[0])
			if err != nil {
				log.Printf("Error deregistering %v from %v", instancesToTerminate[0], tgARN)
				time.Sleep(errorCooldown)
				continue
			}
			log.Printf("Deregistered instance with ID: %v from %v successfully!", instancesToTerminate[0], tgARN)

			instanceInfo, err := instanceService.TerminateInstances(ctx, instancesToTerminate)
			if err != nil {
				log.Printf("Error starting instance %v", err)
				continue
			}
			log.Printf("Terminated instance with ID: %v", aws.ToString(instanceInfo.TerminatingInstances[0].InstanceId))
			time.Sleep(launchCooldown)
			continue
		}
		log.Printf("Maintaining capacity and sleeping for 30s")
		time.Sleep(30 * time.Second)
	}

}

func (s *InstanceService) LaunchInstance(ctx context.Context, instanceCreationInput instanceCreationInput) (string, error) {
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
		return "", fmt.Errorf("launching EC2 instance with AMI %q: %w", aws.ToString(instanceCreationInput.imageID), err)
	}

	if len(runInstancesOutput.Instances) == 0 {
		return "", fmt.Errorf("no %q instance created", aws.ToString(instanceCreationInput.imageID))
	}

	// TODO: Step 4 - Inspect the returned reservation output to extract and return the instance ID string.
	return aws.ToString(runInstancesOutput.Instances[0].InstanceId), nil
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

func (s *InstanceService) TerminateInstances(ctx context.Context, instanceIds []string) (*ec2.TerminateInstancesOutput, error) {

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
					Period: aws.Int32(300),
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

func (s *elbService) deRegisterTarget(ctx context.Context, targetGroupARN string, instanceId string) error {
	// TODO: Step 1 - Build &elasticloadbalancingv2.RegisterTargetsInput
	deRegisterTargetInput := &elasticloadbalancingv2.DeregisterTargetsInput{
		TargetGroupArn: aws.String(targetGroupARN),
		Targets: []elbtypes.TargetDescription{{
			Id:               &instanceId,
			AvailabilityZone: nil,
		}},
	}
	// TODO: Step 2 - Call s.elbClient.RegisterTargets(ctx, registerTargetsInput)
	_, err := s.elbClient.DeregisterTargets(ctx, deRegisterTargetInput)
	// TODO: Step 3 - Line of sight error handling with storytelling wrapping:
	//       fmt.Errorf("registering instance %q to target group %q: %w", instanceID, targetGroupARN, err)
	if err != nil {
		return fmt.Errorf("deregistering instance %q from target group %q: %w", instanceId, targetGroupARN, err)
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
