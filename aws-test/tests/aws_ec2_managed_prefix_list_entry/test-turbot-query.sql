select title
from aws_ec2_managed_prefix_list_entry
where prefix_list_id = '{{ output.prefix_list_id.value }}';
