// Copyright (c) 2018 SAP SE or an SAP affiliate company. All rights reserved. This file is licensed under the Apache Software License, v. 2 except as noted otherwise in the LICENSE file
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package alicloudbotanist

import (
	"github.com/aliyun/aliyun-oss-go-sdk/oss"
	"github.com/gardener/gardener/pkg/client/alicloud"
	"github.com/gardener/gardener/pkg/operation/common"
	"github.com/gardener/gardener/pkg/operation/terraformer"
	"github.com/gardener/gardener/pkg/utils/secrets"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

// DeployInfrastructure kicks off a Terraform job which deploys the infrastructure.
func (b *AlicloudBotanist) DeployInfrastructure() error {
	var (
		err error

		createVPC    = true
		vpcID        = "${alicloud_vpc.vpc.id}"
		natGatewayID = "${alicloud_nat_gateway.nat_gateway.id}"
		snatTableID  = "${alicloud_nat_gateway.nat_gateway.snat_table_ids}"
		vpcCIDR      string
	)

	// check if we should use an existing VPC or create a new one
	if b.Shoot.Info.Spec.Cloud.Alicloud.Networks.VPC.ID != nil {
		createVPC = false
		vpcID = *b.Shoot.Info.Spec.Cloud.Alicloud.Networks.VPC.ID
		if vpcCIDR, err = b.AlicloudClient.GetCIDR(vpcID); err != nil {
			return err
		}

		if natGatewayID, snatTableID, err = b.AlicloudClient.GetNatGatewayInfo(vpcID); err != nil {
			return err
		}
	} else {
		vpcCIDR = string(*b.Shoot.Info.Spec.Cloud.Alicloud.Networks.VPC.CIDR)
	}

	tf, err := b.NewShootTerraformer(common.TerraformerPurposeInfra)
	if err != nil {
		return err
	}

	vals, err := b.generateTerraformInfraConfig(createVPC, vpcID, natGatewayID, snatTableID, vpcCIDR)
	if err != nil {
		return err
	}

	return tf.SetVariablesEnvironment(b.generateTerraformInfraVariablesEnvironment()).
		InitializeWith(b.ChartInitializer("alicloud-infra", vals)).
		Apply()
}

// DestroyInfrastructure kicks off a Terraform job which destroys the infrastructure.
func (b *AlicloudBotanist) DestroyInfrastructure() error {
	tf, err := b.NewShootTerraformer(common.TerraformerPurposeInfra)
	if err != nil {
		return err
	}

	return tf.SetVariablesEnvironment(b.generateTerraformInfraVariablesEnvironment()).
		Destroy()
}

// DeployBackupInfrastructure kicks off a Terraform job which deploys the infrastructure resources for backup.
// It sets up the User and the Bucket to store the backups. Allocate permission to the User to access the bucket.
func (b *AlicloudBotanist) DeployBackupInfrastructure() error {
	tf, err := b.NewBackupInfrastructureTerraformer()
	if err != nil {
		return err
	}
	return tf.
		SetVariablesEnvironment(b.generateTerraformBackupVariablesEnvironment()).
		InitializeWith(b.ChartInitializer("alicloud-backup", b.generateTerraformBackupConfig())).
		Apply()
}

// DestroyBackupInfrastructure kicks off a Terraform job which destroys the infrastructure for etcd backup.
func (b *AlicloudBotanist) DestroyBackupInfrastructure() error {
	tf, err := b.NewBackupInfrastructureTerraformer()
	if err != nil {
		return err
	}

	// Must clean snapshots before deleting the bucket
	stateVariables, err := tf.GetStateOutputVariables(BucketName, StorageEndpoint)
	if err != nil {
		if terraformer.IsVariablesNotFoundError(err) {
			b.Logger.Infof("Skipping Alicloud backup storage bucket deletion because no storage endpoint has been found in the Terraform state.")
			return nil
		}
		return err
	}

	err = cleanSnapshots(stateVariables[BucketName], stateVariables[StorageEndpoint],
		string(b.Seed.Secret.Data[AccessKeyID]), string(b.Seed.Secret.Data[AccessKeySecret]))
	if err != nil {
		return err
	}

	// Clean the bucket using terraformer
	return tf.
		SetVariablesEnvironment(b.generateTerraformBackupVariablesEnvironment()).
		Destroy()
}

// generateTerraformInfraVariablesEnvironment generates the environment containing the credentials which
// are required to validate/apply/destroy the Terraform configuration. These environment must contain
// Terraform variables which are prefixed with TF_VAR_.
func (b *AlicloudBotanist) generateTerraformInfraVariablesEnvironment() map[string]string {
	return terraformer.GenerateVariablesEnvironment(b.Shoot.Secret, map[string]string{
		"ACCESS_KEY_ID":     AccessKeyID,
		"ACCESS_KEY_SECRET": AccessKeySecret,
	})
}

// generateTerraformInfraConfig creates the Terraform variables and the Terraform config (for the infrastructure)
// and returns them (these values will be stored as a ConfigMap and a Secret in the Garden cluster.
func (b *AlicloudBotanist) generateTerraformInfraConfig(createVPC bool, vpcID, natGatewayID, snatTableID, vpcCIDR string) (map[string]interface{}, error) {
	chargeType, err := b.fetchEIPInternetChargeType()
	if err != nil {
		return nil, err
	}

	var (
		sshSecret = b.Secrets["ssh-keypair"]
		zones     = []map[string]interface{}{}
	)

	for idx, zone := range b.Shoot.Info.Spec.Cloud.Alicloud.Zones {
		zones = append(zones, map[string]interface{}{
			"name": zone,
			"cidr": map[string]interface{}{
				"worker": b.Shoot.Info.Spec.Cloud.Alicloud.Networks.Workers[idx],
			},
		})
	}

	return map[string]interface{}{
		"alicloud": map[string]interface{}{
			"region": b.Shoot.Info.Spec.Cloud.Region,
		},
		"create": map[string]interface{}{
			"vpc": createVPC,
		},
		"vpc": map[string]interface{}{
			"cidr":               vpcCIDR,
			"id":                 vpcID,
			"natGatewayID":       natGatewayID,
			"snatTableID":        snatTableID,
			"internetChargeType": chargeType,
		},
		"clusterName":  b.Shoot.SeedNamespace,
		"sshPublicKey": string(sshSecret.Data[secrets.DataKeySSHAuthorizedKeys]),
		"zones":        zones,
	}, nil
}

func (b *AlicloudBotanist) fetchEIPInternetChargeType() (string, error) {
	var (
		vpcID = "vpc_id"
	)
	tf, err := b.NewShootTerraformer(common.TerraformerPurposeInfra)
	if err != nil {
		return "", err
	}
	stateVariables, err := tf.GetStateOutputVariables(vpcID)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return alicloud.DefaultInternetChargeType, nil
		}
		return "", err
	}

	return b.AlicloudClient.GetEIPInternetChargeType(stateVariables[vpcID])
}

func (b *AlicloudBotanist) generateTerraformBackupVariablesEnvironment() map[string]string {
	return terraformer.GenerateVariablesEnvironment(b.Seed.Secret, map[string]string{
		"ACCESS_KEY_ID":     AccessKeyID,
		"ACCESS_KEY_SECRET": AccessKeySecret,
	})
}

func (b *AlicloudBotanist) generateTerraformBackupConfig() map[string]interface{} {
	return map[string]interface{}{
		"alicloud": map[string]interface{}{
			"region": b.Seed.Info.Spec.Cloud.Region,
		},
		"bucket": map[string]interface{}{
			"name": b.Operation.BackupInfrastructure.Name,
		},
	}
}

func cleanSnapshots(bucketName, storageEndpoint, accessKeyID, accessKeySecret string) error {
	client, err := oss.New(storageEndpoint, accessKeyID, accessKeySecret)
	if err != nil {
		return err
	}

	bucket, err := client.Bucket(bucketName)
	if err != nil {
		return err
	}

	for {
		var snapshots []string
		lsRes, err := bucket.ListObjects()
		if err != nil {
			return err
		}
		for _, object := range lsRes.Objects {
			snapshots = append(snapshots, object.Key)
		}
		_, err = bucket.DeleteObjects(snapshots)
		if err != nil {
			return err
		}
		if !lsRes.IsTruncated {
			break
		}
	}
	return nil
}
