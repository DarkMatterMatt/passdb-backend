require 'active_record'
require 'pry-byebug'

require_relative 'database'
require_relative '../models'

ActiveRecord::Base.logger = Logger.new(STDOUT)
puts 'Begin Query Interface'
binding.pry
