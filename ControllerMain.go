package main

import (
	"context"
	"fmt"
	"log"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

type InstanceService struct {
	ec2Client *ec2.Client
}

func main() {

	// TODO: Step 1 - Load config via config.LoadDefaultConfig(ctx)
	ctx := context.Background()
	//awsConfig, configError := config.LoadDefaultConfig(ctx)
	awsConfig, configError := config.LoadDefaultConfig(ctx)

	// TODO: Step 2 - Check err; if err != nil, exit with log.Fatalf(...)
	if configError != nil {
		log.Fatalf("AWS config loading failed with error %v", configError)
	}

	// TODO: Step 3 - Create ec2Client := ec2.NewFromConfig(awsConfig)
	ec2Client := ec2.NewFromConfig(awsConfig)
	// TODO: Step 4 - Instantiate &InstanceService{ec2Client: ec2Client}
	instanceService := &InstanceService{
		ec2Client: ec2Client,
	}
	// TODO: Step 5 - Call instanceService.LaunchInstance(...) and print the instance ID!
	instanceID, err := instanceService.LaunchInstance(ctx, "ami-0f8a61b66d1accaee", types.InstanceTypeT2Micro, "TagInstanceTest")

	if err != nil {
		log.Fatalf("failed to launch instance %s", err)
	}

	log.Printf("Launched instance: %s", instanceID)

	instanceIDOutput, err := instanceService.GetInstancesByTag(ctx, "Name", "TagInstanceTest")

	if err != nil{
		log.Fatalf("Error retrieving instances", err)
	}

	for i, ID := range instanceIDOutput{
		fmt.Printf("Instance %v has ID %v \n", i, ID)
	}
}

func (s *InstanceService) LaunchInstance(ctx context.Context, imageID string, instanceType types.InstanceType, instanceTag string) (string, error) {
	// TODO: Step 1 - Construct &ec2.RunInstancesInput with explicit, multi-line named fields:
	//       - ImageId (use aws.String)
	//       - InstanceType (the types.InstanceType passed in)
	//       - MinCount & MaxCount (use aws.Int32)

	runInstanceInput := &ec2.RunInstancesInput{
		ImageId:      &imageID,
		InstanceType: instanceType,
		MinCount:     aws.Int32(1),
		MaxCount:     aws.Int32(1),
		TagSpecifications: []types.TagSpecification{
			{
				ResourceType: types.ResourceTypeInstance,
				Tags:         []types.Tag{{Key: aws.String("Name"), Value: &instanceTag}},
			},
		},
	}

	// TODO: Step 2 - Call s.ec2Client.RunInstances(ctx, runInstancesInput)

	runInstancesOutput, err := s.ec2Client.RunInstances(ctx, runInstanceInput)

	// TODO: Step 3 - Check error immediately with early return ("Line of sight").
	//       Wrap the error using: fmt.Errorf("launching EC2 instance with AMI %q: %w", imageID, err)
	if err != nil {
		return "", fmt.Errorf("launching EC2 instance with AMI %q: %w", imageID, err)
	}

	if len(runInstancesOutput.Instances) == 0 {
		return "", fmt.Errorf("no %q instance created", imageID)
	}

	fmt.Println("Successfully tagged instance")

	// TODO: Step 4 - Inspect the returned reservation output to extract and return the instance ID string.
	return aws.ToString(runInstancesOutput.Instances[0].InstanceId), nil
}

func (s *InstanceService) GetInstancesByTag(ctx context.Context, tagName string, tagValue string) ([]string, error) {
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
		return nil, fmt.Errorf("Something went wrong while looking for instances", err)
	}
	// TODO: Step 4 - Initialize a slice: var instanceIDs []string
	var instanceIDs []string
	// TODO: Step 5 - Range over Reservations and Instances, appending aws.ToString(inst.InstanceId)
	for _, reservtion := range describeInstanceOutput.Reservations {
		for _, instance := range reservtion.Instances {
			instanceIDs = append(instanceIDs, *instance.InstanceId)
		}
	}
	// TODO: Step 6 - Return instanceIDs and nil
	return instanceIDs, nil
}
