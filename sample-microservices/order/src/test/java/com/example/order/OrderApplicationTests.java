package com.example.order;

import org.junit.jupiter.api.Test;
import org.springframework.boot.test.context.SpringBootTest;

import com.example.order.commons.utils;

@SpringBootTest
class OrderApplicationTests {

	@Test
	void contextLoads() {


	}
	@Test
	void testGetOrders(){
		String expectedBodyString = "hello";
		// Test the /orders endpoint
		String url="http://localhost:8085/orders/";
		String method="GET";
		com.example.order.models.Response response=utils.test("8085",url,method);
		// Verify the response
		assert(response.getBody().equals(expectedBodyString));
	}

}
