# How to access resources allocated by Cluster-Bot on AWS
####
This document explains how to get direct access to resource allocated by cluster-bot.
####
### Steps

- retrieve AWS credentials used by *cluster-bot* to provision the cluster.
```shell
  oc get secrets aws-creds -n kube-system -o yaml
```
- create a profile for the cluster-bot within your local AWS.
  * ```shell
      # add a section for this profile in ~/.aws/config
      # add a section for this profile in ~/.aws/credentials
      aws configure --profile bot
    ```
  * ```shell
      # check that profile has been created
      aws configure list-profiles
    ```  
  * ```shell
      # retrieve aws_access_key_id used by cluster bot 
      # decode it using base64
      echo "aws_access_key_id from aws-creds secret" | base64 --decode
    ```
  * ```shell
      # retrieve aws_secret_access_key used by cluster bot 
      # decode it using base64
      echo "aws_secret_access_key from aws-creds secret" | base64 --decode
    ``` 
  * ```shell
      # make sure ~/.aws/credentials contains the following section 
      [bot]
      aws_access_key_id = base64_decoded_token
      aws_secret_access_key = base64_decoded_token
    ```         
  * ```shell
      # make sure ~/.aws/config contains the following section 
      [profile bot]
      region=us-west-1 # update this according to the provisioned cluster region.
      output=text #
    ```          
  * ```shell
      # you can find the cluster region within any yaml of any resource 
      oc get nodes -o wide
    ```   
- use aws_cli to access the resources.
  * ```shell
      aws ec2 describe-instances --profile bot --output json
    ```   
     