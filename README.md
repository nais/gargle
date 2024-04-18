# GARgle

> While gargle itself doesn't directly mean cleaning, it involves using a liquid to wash or cleanse the throat. The sound "gar" can be associated with the action of gargling, making it a good fit for your description.

## How it works

In general, Gargle works by managing a tag for images in use in the cluster.
The tag Gargle uses is in the format `keep-nais-[name]-[calculated-sha]`.

### 1. Fetch registries

Gargle starts by getting all artifact registries from the provided `management-project-id` project.
It filters out the ones that are not a standard docker registry, and are not tagged with the `team` tag.

### 2. Fetch images

It the fetches all images from the cluster, using the configuration in `config.yaml`.

It uses a dynamic approach to get the images, so that it in theory should work with any kind of CRD.

Take the following snippet as an example:

```yaml
images-from:
  apps/v1:
    - resource: replicasets
      jsonpath: $.spec.template.spec.containers[*].image
```

This configuration will get all images from all replicasets in the cluster.

The source list is filtered to only include images that are in the registries fetched in the previous step.

### 3. Tag images

Gargle will iterate through every registry from step 1 and verify that all images with a `keep-nais-[name]` is still in use, removing the tag if it is not.

It will also tag all images from step 2 with the `keep-nais-[name]-[calculated-sha]` tag.

## Deleting images

This service is designed to be used with [Google Artifact Registry Cleanup Policy](https://cloud.google.com/artifact-registry/docs/repositories/cleanup-policy).

Create a policy which deletes images older than some time, and uses the `keep-nais-` tag as an exception.

E.g.

```json
{
  "name": "keep-in-use",
  "action": { "type": "Keep" },
  "condition": {
    "tagState": "TAGGED",
    "tagPrefixes": ["keep-nais-"]
  }
}
```

## Testing locally

To test locally, you can copy the `config.yaml.example` to `config.yaml` and fill in the values.

Then you can run the following command:

```shell
gcloud auth login --update-adc
go run main.go
```

## Performance

Substituting the actuall tagging and untagging for a sleep 50ms, the service spent about 6.5 minutes to update `dev-gcp` (there's no images to untag).

```
Got repositories in 1.342174627s
Gathered images in 9.68557442s
Tagged images in 6m16.193608617s

```

Some optimizations could be made, e.g. keep a list of tagged images from the untagging step instead of trying to apply the tag to all images.

The amount of memory used atm can probably be reduced.
