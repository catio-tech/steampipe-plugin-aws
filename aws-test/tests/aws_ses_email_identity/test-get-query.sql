select
  identity,
  arn,
  region,
  akas
from
  aws_ses_email_identity
where identity = '{{resourceName}}';
