connection "catio_dev" {
	  plugin = "local/aws"
	  profile = "catio-dev"
	  regions = ["us-west-2"]
}

connection "catio_prod" {
      plugin = "local/aws"
      profile = "catio-prod"
      regions = ["us-west-2"]
}
