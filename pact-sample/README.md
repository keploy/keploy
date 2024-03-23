# Sample Pact to demonstrate Contract Testing (SpringBoot)


## Architecture
<ul>
<li>
Assume we have three microservices "Customer" and "Product" (Consumer) and Retailer which is Provider.
</li>

<li>
<a href="https://ibb.co/gD40BHH"><img src="https://i.ibb.co/4j1b599/arc2.png" alt="arc2" border="0"></a>
</li>

</ul>

## Flow:

## While Product Service test against Mock Provider:


### Using Mockito to mock the services.
#### Contract is Created and published to pact broker


<ul><li>
Contract is published but not verified by retailer service (provider):
<hr>
<a href="https://ibb.co/gD51vS2"><img src="https://i.ibb.co/P57pTcL/publishnotverified.png" alt="publishnotverified" border="0"></a></li>

<li>
Retailer then verify the contract in the test:

<hr>
<a href="https://ibb.co/FhdqnHg"><img src="https://i.ibb.co/zmtHN6G/verified.png" alt="verified" border="0"></a>
</li>

<li>
Assume we change any field name in provider test will cause the contract to be unverifed <hr>
<a href="https://ibb.co/pRVBS11"><img src="https://i.ibb.co/nndNFww/afterparamchange.png" alt="afterparamchange" border="0"></a>
<hr>
<a href="https://ibb.co/z488rTZ"><img src="https://i.ibb.co/tJZZ4TD/changed-Param.png" alt="changed-Param" border="0"></a>

</li>

</ul>

#### Same steps can be done with customer services and contract created then verifed by retailer
<ul>

<li>
<hr>
<a href="https://ibb.co/J21gBj5"><img src="https://i.ibb.co/vmFRw4Z/customer.png" alt="customer" border="0"></a></li>

</ul>

### That's it!