# S3 backend for remote state — use partial configuration at init time.
#
# For PR preview environments:
#   terraform init \
#     -backend-config="bucket=$TF_STATE_BUCKET" \
#     -backend-config="key=pr-<number>/terraform.tfstate" \
#     -backend-config="dynamodb_table=$TF_LOCK_TABLE" \
#     -backend-config="region=us-east-1"
#
# For local development (no remote state):
#   Remove or comment out this block and use local state.

terraform {
  backend "s3" {
    # All values supplied via -backend-config at init time.
    # bucket         = "..."
    # key            = "..."
    # region         = "..."
    # dynamodb_table = "..."
    encrypt = true
  }
}
